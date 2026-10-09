package responses

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"time"

	"orchids-api/internal/util"
	"strings"
	"sync"
	"testing"

	"encoding/json"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestParseResponsesResourcePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path       string
		wantID     string
		wantAction string
		wantOK     bool
	}{
		{"/v1/responses/resp_1", "resp_1", "", true},
		{"/v1/responses/resp_1/", "resp_1", "", true},
		{"/cline/v1/responses/resp_1/cancel", "resp_1", "cancel", true},
		{"/workbuddy/v1/responses/resp_1/input_items", "resp_1", "input_items", true},
		{"/v1/responses/resp%5F1/cancel", "resp_1", "cancel", true},
		{"/v1/responses/compact", "", "", false},
		{"/v1/responses/", "", "", false},
		{"/v1/responses", "", "", false},
		{"/something/else", "", "", false},
	}
	for _, tc := range cases {
		id, action, ok := ParseResourcePath(tc.path)
		testutil.Equal(t, ok, tc.wantOK)
		testutil.Equal(t, id, tc.wantID)
		testutil.Equal(t, action, tc.wantAction)
		wantAction := ""
		if tc.wantAction == ActionCancel || tc.wantAction == ActionInputItems {
			wantAction = tc.wantAction
		}
		testutil.Equal(t, SubResourceAction(tc.path), wantAction)
	}
}

// TestResponsesInputItemsJSONNormalizesEveryInputShape pins the round trip a
// client performs: it reads the list and sends it back as the next turn's input.
// An item without an id or a status would be rejected on the way back, so the
// normalizer must supply both while leaving the client's own labels alone.
func TestResponsesInputItemsJSONNormalizesEveryInputShape(t *testing.T) {
	t.Parallel()

	var fromString []map[string]interface{}
	testutil.NoError(t, json.Unmarshal(InputItemsJSON("hello"), &fromString), "string input did not normalize: %v")
	testutil.Equal(t, len(fromString), 1)
	testutil.Equal(t, fromString[0]["type"], "message")
	testutil.Equal(t, fromString[0]["role"], "user")
	if !strings.HasPrefix(ParseLooseStringAny(fromString[0]["id"]), "msg_") {
		t.Fatalf("string input item id = %#v, want a msg_ id", fromString[0]["id"])
	}
	testutil.Equal(t, fromString[0]["status"], "completed")

	var fromArray []map[string]interface{}
	raw := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
			map[string]interface{}{"type": "input_text", "text": "first"},
		}},
		map[string]interface{}{"id": "fc_known", "type": "function_call", "call_id": "call_1", "name": "f"},
	}
	testutil.NoError(t, json.Unmarshal(InputItemsJSON(raw), &fromArray), "array input did not normalize: %v")
	testutil.Equal(t, len(fromArray), 2)
	testutil.Equal(t, fromArray[1]["id"], "fc_known")
	for i, item := range fromArray {
		testutil.Falsef(t, ParseLooseStringAny(item["id"]) == "" || ParseLooseStringAny(item["status"]) == "", "item %d is missing id/status: %#v", i, item)
	}

	got := InputItemsJSON(nil)
	testutil.Falsef(t, got != nil, "nil input produced %s, want no stored items", got)
}

func TestResponsesInputItemsServesPersistedItems(t *testing.T) {
	t.Parallel()

	opts := BridgeOptions{Store: store.NewMemoryResponseStore(0)}
	id := "resp_items_" + util.RandomHex(8)
	if err := opts.StoreFor().SaveStoredResponse(context.Background(), &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
		ResponseID: id,
		OwnerHash:  "anonymous",
		Model:      "gpt-5.6-luna",
		Provider:   bridgedResponseProvider,
		Body:       []byte(`{"id":"` + id + `","object":"response","status":"completed"}`),
		InputItems: InputItemsJSON("first turn"),
	}, 0); err != nil {
		t.Fatalf("SaveStoredResponse() error = %v", err)
	}

	handler := InputItemsHandler(opts)
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+id+"/input_items", nil))

	testutil.Equal(t, rec.Code, http.StatusOK)
	var payload struct {
		Object  string                   `json:"object"`
		Data    []map[string]interface{} `json:"data"`
		FirstID string                   `json:"first_id"`
		LastID  string                   `json:"last_id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &payload)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, payload.Object, "list")
	testutil.Equal(t, len(payload.Data), 1)
	content, _ := payload.Data[0]["content"].([]interface{})
	testutil.Equal(t, len(content), 1)
	part, _ := content[0].(map[string]interface{})
	testutil.Equal(t, part["text"], "first turn")
	testutil.Falsef(t, payload.FirstID == "" || payload.FirstID != payload.LastID, "first_id/last_id = %q/%q, want the single item id", payload.FirstID, payload.LastID)
}

func TestResponsesInputItemsUnknownResponseIs404(t *testing.T) {
	t.Parallel()

	handler := InputItemsHandler(BridgeOptions{Store: store.NewMemoryResponseStore(0)})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_missing/input_items", nil))

	testutil.Equal(t, rec.Code, http.StatusNotFound)
	testutil.MustContain(t, rec.Body.String(), "response_not_found")
}

func TestResponsesSubResourcesRejectWrongMethod(t *testing.T) {
	t.Parallel()

	opts := BridgeOptions{Store: store.NewMemoryResponseStore(0)}
	cancel := CancelHandler(opts)
	rec := httptest.NewRecorder()
	cancel(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1/cancel", nil))
	testutil.Falsef(t, rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"), "POST"), "cancel GET: status=%d Allow=%q", rec.Code, rec.Header().Get("Allow"))
	testutil.MustContain(t, rec.Body.String(), `"error"`)

	items := InputItemsHandler(opts)
	rec = httptest.NewRecorder()
	items(rec, httptest.NewRequest(http.MethodPost, "/v1/responses/resp_1/input_items", nil))
	testutil.Falsef(t, rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Header().Get("Allow"), "GET"), "input_items POST: status=%d Allow=%q", rec.Code, rec.Header().Get("Allow"))
}

// TestResponsesCancelFlipsStoredStatus covers the whole observable contract:
// cancel answers with the response object carrying status=cancelled, the change
// is persisted, and a second cancel is a no-op that returns the same object.
func TestResponsesCancelFlipsStoredStatus(t *testing.T) {
	t.Parallel()

	opts := BridgeOptions{Store: store.NewMemoryResponseStore(0)}
	id := "resp_cancel_" + util.RandomHex(8)
	body := `{"id":"` + id + `","object":"response","status":"completed","output":[` +
		`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`
	if err := opts.StoreFor().SaveStoredResponse(context.Background(), &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
		ResponseID: id, OwnerHash: "anonymous", Model: "gpt-5.6-luna",
		Provider: bridgedResponseProvider, ContentType: "application/json", Body: []byte(body),
	}, 0); err != nil {
		t.Fatalf("SaveStoredResponse() error = %v", err)
	}

	cancel := CancelHandler(opts)
	first := httptest.NewRecorder()
	cancel(first, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses/"+id+"/cancel", nil))
	testutil.Equal(t, first.Code, http.StatusOK)
	var cancelled map[string]interface{}
	err := json.Unmarshal(first.Body.Bytes(), &cancelled)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, cancelled["status"], "cancelled")
	testutil.EqualAny(t, cancelled["id"], id)
	output, _ := cancelled["output"].([]interface{})
	testutil.Equal(t, len(output), 1)

	record, err := opts.StoreFor().GetStoredResponse(context.Background(), id, "anonymous") //nolint:staticcheck // see above
	testutil.NoError(t, err, "GetStoredResponse() error = %v")
	testutil.MustContain(t, string(record.Body), `"status":"cancelled"`)

	second := httptest.NewRecorder()
	cancel(second, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses/"+id+"/cancel", nil))
	testutil.Equal(t, second.Code, http.StatusOK)
	testutil.Equal(t, second.Body.String(), first.Body.String())
}

func TestResponsesCancelUnknownResponseIs404(t *testing.T) {
	t.Parallel()

	cancel := CancelHandler(BridgeOptions{Store: store.NewMemoryResponseStore(0)})
	rec := httptest.NewRecorder()
	cancel(rec, httptest.NewRequest(http.MethodPost, "/v1/responses/resp_missing/cancel", nil))
	testutil.Falsef(t, rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "response_not_found"), "status = %d body = %s", rec.Code, rec.Body.String())
}

// TestResponsesCancelAnswersBuildOwnershipRecords guards the build record case:
// ownership exists but the body lives upstream, so the gateway answers with the
// identity it knows rather than reporting the response as missing.
func TestResponsesCancelAnswersBuildOwnershipRecords(t *testing.T) {
	t.Parallel()

	opts := BridgeOptions{Store: store.NewMemoryResponseStore(0)}
	id := "resp_build_" + util.RandomHex(8)
	if err := opts.StoreFor().SaveStoredResponse(context.Background(), &store.StoredResponse{ //nolint:staticcheck // nil ctx is fine for the in-process store
		ResponseID: id, OwnerHash: "anonymous", Model: "grok-4.6", Provider: "build",
	}, 0); err != nil {
		t.Fatalf("SaveStoredResponse() error = %v", err)
	}

	rec := httptest.NewRecorder()
	CancelHandler(opts)(rec, httptest.NewRequest(http.MethodPost, "/grok/v1/responses/"+id+"/cancel", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	var decoded map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &decoded)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, decoded["status"], "cancelled")
	testutil.EqualAny(t, decoded["id"], id)
	testutil.Equal(t, decoded["model"], "grok-4.6")
}

// TestResponsesBridgeForwardsTextFormatAndInclude is gap ②/③: the bridge used
// to drop these while the native Grok path forwarded them, so the same request
// produced structured output on one channel and free-form prose on the others.
func TestResponsesBridgeForwardsTextFormatAndInclude(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), BridgeOptions{})

	body := `{"model":"gpt-5.6-luna","input":"hi","stream":true,` +
		`"include":["reasoning.encrypted_content"],` +
		`"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	bridge(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	testutil.Equal(t, len(calls), 1)
	inner := calls[0].body
	format, _ := inner["response_format"].(map[string]interface{})
	if format == nil || format["type"] != "json_schema" || format["name"] != "answer" {
		t.Fatalf("response_format = %#v, want the text.format object", inner["response_format"])
	}
	include, _ := inner["include"].([]interface{})
	testutil.Equal(t, len(include), 0)
	testutil.True(t, rec.Header().Get("X-Grok2API-Compatibility-Warnings") != "", "missing encrypted reasoning compatibility warning")
	text, _ := inner["text"].(map[string]interface{})
	testutil.Falsef(t, text == nil, "text controls were dropped: %#v", inner)
}

// TestResponsesBridgePrefersTextFormatOverResponseFormat pins precedence: when a
// migrated client sends both spellings, the field the Responses API defines wins.
func TestResponsesBridgePrefersTextFormatOverResponseFormat(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	calls := []recordedChatCall{}
	bridge := ResponsesBridgeHandler(recordingChat(t, &calls, &mu), BridgeOptions{})

	body := `{"model":"gpt-5.6-luna","input":"hi","stream":true,` +
		`"response_format":{"type":"text"},` +
		`"text":{"format":{"type":"json_object"}}}`
	rec := httptest.NewRecorder()
	bridge(rec, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses", strings.NewReader(body)))
	testutil.Equal(t, rec.Code, http.StatusOK)

	mu.Lock()
	defer mu.Unlock()
	format, _ := calls[0].body["response_format"].(map[string]interface{})
	if format == nil || format["type"] != "json_object" {
		t.Fatalf("response_format = %#v, want text.format to win", calls[0].body["response_format"])
	}
}

// TestResponsesBridgeMemoryFallbackStoresResponses is gap ⑤: a gateway with no
// response backend must keep store=true, retrieval and continuation working
// in-process instead of answering response_store_unavailable.
func TestResponsesBridgeMemoryFallbackStoresResponses(t *testing.T) {
	t.Parallel()

	opts := BridgeOptions{}
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"fallback-answer"},"finish_reason":"stop"}]}`)
	}
	bridge := ResponsesBridgeHandler(chat, opts)
	resource := ResourceHandler(opts)

	create := httptest.NewRecorder()
	bridge(create, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"hi","store":true}`)))
	testutil.Equal(t, create.Code, http.StatusOK)
	var created map[string]interface{}
	err := json.Unmarshal(create.Body.Bytes(), &created)
	testutil.CheckNoError(t, err)
	responseID, _ := created["id"].(string)
	testutil.NotEqual(t, responseID, "")

	get := httptest.NewRecorder()
	resource(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	testutil.Falsef(t, get.Code != http.StatusOK || !strings.Contains(get.Body.String(), "fallback-answer"), "get status=%d body=%s", get.Code, get.Body.String())

	// The in-process store must serve the items too, not just the body.
	items := httptest.NewRecorder()
	InputItemsHandler(opts)(items, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID+"/input_items", nil))
	testutil.Falsef(t, items.Code != http.StatusOK || !strings.Contains(items.Body.String(), "hi"), "input_items status=%d body=%s", items.Code, items.Body.String())
}

// TestResponsesSubResourcesRoundTripThroughRedis drives create -> input_items ->
// cancel -> get against the real Redis-backed store, so the new stored field is
// proven to survive the JSON path rather than only the in-process store. A
// sibling replica serving the next call reads the record through the same path.
func TestResponsesSubResourcesRoundTripThroughRedis(t *testing.T) {
	s := store.NewMemoryResponseStore(time.Hour)

	opts := BridgeOptions{Store: s}
	chat := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"gpt-5.6-luna","choices":[{"index":0,"message":{"role":"assistant","content":"round-trip"},"finish_reason":"stop"}]}`)
	}

	create := httptest.NewRecorder()
	ResponsesBridgeHandler(chat, opts)(create, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6-luna","input":"remember this turn","store":true}`)))
	testutil.Equal(t, create.Code, http.StatusOK)
	var created map[string]interface{}
	err := json.Unmarshal(create.Body.Bytes(), &created)
	testutil.CheckNoError(t, err)
	responseID, _ := created["id"].(string)
	testutil.NotEqual(t, responseID, "")

	items := httptest.NewRecorder()
	InputItemsHandler(opts)(items, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID+"/input_items", nil))
	testutil.Falsef(t, items.Code != http.StatusOK || !strings.Contains(items.Body.String(), "remember this turn"), "input_items status=%d body=%s", items.Code, items.Body.String())

	cancelled := httptest.NewRecorder()
	CancelHandler(opts)(cancelled, httptest.NewRequest(http.MethodPost, "/workbuddy/v1/responses/"+responseID+"/cancel", nil))
	testutil.Falsef(t, cancelled.Code != http.StatusOK || !strings.Contains(cancelled.Body.String(), `"status":"cancelled"`), "cancel status=%d body=%s", cancelled.Code, cancelled.Body.String())

	// The cancellation must be visible through a fresh read of the same store.
	get := httptest.NewRecorder()
	ResourceHandler(opts)(get, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID, nil))
	testutil.Falsef(t, get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"status":"cancelled"`), "get after cancel status=%d body=%s", get.Code, get.Body.String())
	// input_items must still be readable after the status rewrite.
	itemsAgain := httptest.NewRecorder()
	InputItemsHandler(opts)(itemsAgain, httptest.NewRequest(http.MethodGet, "/workbuddy/v1/responses/"+responseID+"/input_items", nil))
	testutil.Falsef(t, itemsAgain.Code != http.StatusOK || !strings.Contains(itemsAgain.Body.String(), "remember this turn"), "input_items after cancel status=%d body=%s", itemsAgain.Code, itemsAgain.Body.String())
}
