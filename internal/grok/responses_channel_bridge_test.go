package grok

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"regexp"
	"strings"
	"sync"
	"testing"

	"encoding/json"

	"orchids-api/internal/testutil"
)

func TestResponsesChatPathMapsTheChannelPrefix(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/workbuddy/v1/responses":         "/workbuddy/v1/chat/completions",
		"/workbuddy/v1/responses/compact": "/workbuddy/v1/chat/completions",
		"/cline/v1/responses/":            "/cline/v1/chat/completions",
		"/workbuddy/responses":            "/workbuddy/chat/completions",
		"  /qoder/v1/responses  ":         "/qoder/v1/chat/completions",
		"/something/else":                 "/something/else",
	}
	for path, want := range cases {
		testutil.Equal(t, responsesChatPath(path), want)
	}
}

type recordedChatCall struct {
	path string
	body map[string]interface{}
}

// recordingChat captures the inner chat request and replies with a complete
// chat-completions SSE stream, which is what the shared handler emits.
func recordingChat(t *testing.T, calls *[]recordedChatCall, mu *sync.Mutex) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		testutil.CheckNoError(t, err, "inner chat body: %v")
		var decoded map[string]interface{}
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		*calls = append(*calls, recordedChatCall{path: r.URL.Path, body: decoded})
		mu.Unlock()
		if streaming, _ := decoded["stream"].(bool); !streaming {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","model":"gpt-5.6-luna","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
			`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
		}
	}
}

// Every event on the bridged stream carries a sequence_number that increases by
// one: a client that reconnects with Last-Event-ID asks for everything after the
// last number it saw, so an event without one cannot be ordered at all.
func TestResponsesBridgeStreamNumbersEveryEvent(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), responses.BridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"say hi","stream":true}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	var numbers []int
	seen := 0
	if err := responses.ConsumeSSE(strings.NewReader(rec.Body.String()), func(event responses.SSEEvent) error {
		// The [DONE] terminator is a chat-completions habit, not a Responses
		// event, so it carries no envelope and no number.
		if strings.TrimSpace(string(event.Data())) == "[DONE]" {
			return nil
		}
		seen++
		var payload map[string]interface{}
		if err := json.Unmarshal(event.Data(), &payload); err != nil {
			return fmt.Errorf("event %q: %v", event.Event, err)
		}
		number, ok := payload["sequence_number"].(float64)
		if !ok {
			return fmt.Errorf("event %q carries no sequence_number", event.Event)
		}
		numbers = append(numbers, int(number))
		return nil
	}); err != nil {
		t.Fatalf("consume bridged stream: %v", err)
	}
	testutil.Falsef(t, seen == 0, "the bridge produced no events")
	testutil.Equal(t, len(numbers), seen)
	// The first number is 0 and each following one is exactly one higher, so a
	// client can resume from any of them.
	for i, number := range numbers {
		testutil.Equal(t, number, i)
	}
}

func TestResponsesBridgeStreamsChatAsResponses(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), responses.BridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","instructions":"be brief","input":"say hi","stream":true}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	out := rec.Body.String()
	for _, want := range []string{"event: response.created", "response.output_text.delta", `"hello"`, "event: response.completed"} {
		testutil.MustContain(t, out, want)
	}

	mu.Lock()
	defer mu.Unlock()
	testutil.Equal(t, len(calls), 1)
	testutil.Equal(t, calls[0].path, "/workbuddy/v1/chat/completions")
	messages, _ := calls[0].body["messages"].([]interface{})
	testutil.Equal(t, len(messages), 2)
	first, _ := messages[0].(map[string]interface{})
	testutil.Equal(t, first["role"], "system")
	testutil.Equal(t, first["content"], "be brief")
	if stream, _ := calls[0].body["stream"].(bool); !stream {
		t.Fatalf("inner stream = %#v, want true", calls[0].body["stream"])
	}
}

func TestResponsesBridgeNonStreamReturnsAResponseObject(t *testing.T) {
	t.Parallel()

	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}
	bridge := ResponsesBridgeHandler(chat, responses.BridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"say hi"}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	var decoded map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &decoded)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, decoded["object"], "response")
	testutil.Equal(t, decoded["model"], "gpt-5.6-luna")
	testutil.MustContain(t, rec.Body.String(), "hello")
}

func TestResponsesBridgeForwardsChatErrors(t *testing.T) {
	t.Parallel()

	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found","type":"invalid_request_error"}}`)
	}
	bridge := ResponsesBridgeHandler(chat, responses.BridgeOptions{})

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"does-not-exist","input":"hi","stream":true}`))
	rec := httptest.NewRecorder()
	bridge(rec, req)

	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.MustContain(t, rec.Body.String(), "model not found")
}

func TestResponsesBridgeRejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	bridge := ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("inner chat handler must not run for an invalid request")
	}, responses.BridgeOptions{})

	for name, body := range map[string]string{
		"missing_model": `{"input":"hi"}`,
		"missing_input": `{"model":"gpt-5.6-luna"}`,
		"broken_json":   `{"model":`,
		"background":    `{"model":"gpt-5.6-luna","input":"hi","background":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(body))
			rec := httptest.NewRecorder()
			bridge(rec, req)
			testutil.Equal(t, rec.Code, http.StatusBadRequest)
		})
	}
}

func TestResponsesBridgeRejectsNonPost(t *testing.T) {
	t.Parallel()

	bridge := ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {}, responses.BridgeOptions{})
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses", nil))
	testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
}

func TestResponsesChannelSubpathServesCompactAndTrailingSlash(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	handler := ResponsesChannelSubpath(recordingChat(t, &calls, &mu), responses.BridgeOptions{})

	for name, target := range map[string]string{
		"trailing_slash": "/qoder/v1/responses/",
		"compact":        "/workbuddy/v1/responses/compact",
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(`{"model":"gpt-5.6-luna","input":"summarise the thread","stream":true}`))
			rec := httptest.NewRecorder()
			handler(rec, req)
			testutil.Equal(t, rec.Code, http.StatusOK)
			testutil.MustContain(t, rec.Body.String(), "event: response.completed")
		})
	}

	mu.Lock()
	defer mu.Unlock()
	testutil.Equal(t, len(calls), 2)
	// The sub-tests run in map order, so compare the set of paths.
	paths := map[string]bool{}
	for _, call := range calls {
		paths[call.path] = true
	}
	testutil.Falsef(t, !paths["/qoder/v1/chat/completions"] || !paths["/workbuddy/v1/chat/completions"], "inner chat paths = %v, want the same channel prefix as the request", paths)
}

// The chat-only channels keep no response store, so /responses/{id} must answer
// with the Responses error envelope instead of Go's plain-text 404.
func TestResponsesChannelSubpathReportsUnstoredResponses(t *testing.T) {
	t.Parallel()

	handler := ResponsesChannelSubpath(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("the create handler must not serve a resource path")
	}, responses.BridgeOptions{})

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "/cline/v1/responses/resp_123", nil)
		rec := httptest.NewRecorder()
		handler(rec, req)
		testutil.Equal(t, rec.Code, http.StatusNotFound)
		testutil.MustContain(t, rec.Body.String(), "response_not_found")
	}

	put := httptest.NewRequest(http.MethodPut, "/cline/v1/responses/resp_123", nil)
	rec := httptest.NewRecorder()
	handler(rec, put)
	testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
	allow := rec.Header().Get("Allow")
	testutil.Falsef(t, !strings.Contains(allow, "GET") || !strings.Contains(allow, "DELETE"), "Allow = %q, want GET and DELETE", allow)
}

func TestResponsesBridgeStoresAndServesResponses(t *testing.T) {
	_, s, _ := setupValidationHandler(t)

	var mu sync.Mutex
	var chatBodies []map[string]interface{}
	chat := func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]interface{}
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		chatBodies = append(chatBodies, decoded)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-9","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"stored-answer"},"finish_reason":"stop"}]}`)
	}
	opts := responses.BridgeOptions{Store: s}
	bridge := ResponsesBridgeHandler(chat, opts)
	resource := responses.ResourceHandler(opts)

	create := httptest.NewRecorder()
	bridge(create, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"hi","store":true}`)))
	testutil.Equal(t, create.Code, http.StatusOK)
	var created map[string]interface{}
	err := json.Unmarshal(create.Body.Bytes(), &created)
	testutil.CheckNoError(t, err)
	responseID, _ := created["id"].(string)
	testutil.Falsef(t, !strings.HasPrefix(responseID, "resp_"), "response id = %q, want a resp_ id", responseID)

	get := httptest.NewRecorder()
	resource(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	testutil.Falsef(t, get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "stored-answer"), "get status=%d body=%s", get.Code, get.Body.String())

	// A continuation must replay the stored conversation upstream.
	continuation := httptest.NewRecorder()
	bridge(continuation, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"again","previous_response_id":"`+responseID+`"}`)))
	testutil.Equal(t, continuation.Code, http.StatusOK)
	mu.Lock()
	last := chatBodies[len(chatBodies)-1]
	mu.Unlock()
	testutil.MustContain(t, fmt.Sprint(last["messages"]), "stored-answer")

	deleted := httptest.NewRecorder()
	resource(deleted, httptest.NewRequest(http.MethodDelete, "/workbuddy/v1/responses/"+responseID, nil))
	testutil.Falsef(t, deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`), "delete status=%d body=%s", deleted.Code, deleted.Body.String())
	gone := httptest.NewRecorder()
	resource(gone, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	testutil.Equal(t, gone.Code, http.StatusNotFound)
}

func TestResponsesBridgeStoresStreamedResponse(t *testing.T) {
	_, s, _ := setupValidationHandler(t)

	var mu sync.Mutex
	calls := []recordedChatCall{}
	opts := responses.BridgeOptions{Store: s}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), opts)
	resource := responses.ResourceHandler(opts)

	stream := httptest.NewRecorder()
	bridge(stream, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5-6-sol-low","input":"hi","stream":true,"store":true}`)))
	testutil.Falsef(t, stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), "event: response.completed"), "stream status=%d body=%s", stream.Code, stream.Body.String())
	match := regexp.MustCompile(`"id":"(resp_[0-9a-f]+)"`).FindStringSubmatch(stream.Body.String())
	testutil.Falsef(t, len(match) < 2, "stream carries no response id: %s", stream.Body.String())

	get := httptest.NewRecorder()
	resource(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+match[1], nil))
	testutil.Falsef(t, get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "hello"), "stored streamed response status=%d body=%s", get.Code, get.Body.String())
}
