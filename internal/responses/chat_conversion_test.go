package responses

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/testutil"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResponsesObjectFromChat_ConvertsMessageAndToolCalls(t *testing.T) {
	chat := map[string]interface{}{
		"model": "grok-4.20-0309",
		"choices": []interface{}{map[string]interface{}{
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": "answer",
				"annotations": []interface{}{map[string]interface{}{
					"type":         "url_citation",
					"url_citation": map[string]interface{}{"url": "https://example.com"},
				}},
				"tool_calls": []interface{}{map[string]interface{}{
					"id":   "call_1",
					"type": "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"city":"Shanghai"}`,
					},
				}},
			},
		}},
		"usage": map[string]interface{}{"prompt_tokens": float64(3), "completion_tokens": float64(4), "total_tokens": float64(7)},
	}

	resp := ObjectFromChat("grok-4.20-0309", chat)
	testutil.Equal(t, resp["object"], "response")
	testutil.Equal(t, resp["status"], "completed")
	output := resp["output"].([]interface{})
	testutil.Equal(t, len(output), 2)
	fc := output[0].(map[string]interface{})
	testutil.Falsef(t, fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather", "function_call mismatch: %#v", fc)
	msg := output[1].(map[string]interface{})
	content := msg["content"].([]interface{})[0].(map[string]interface{})
	testutil.Equal(t, content["text"], "answer")
	usage := resp["usage"].(map[string]interface{})
	testutil.Falsef(t, usage["input_tokens"] != 3 || usage["output_tokens"] != 4 || usage["total_tokens"] != 7, "usage mismatch: %#v", usage)
}

func TestWriteResponsesStreamFromChat_ConvertsToolCallChunk(t *testing.T) {
	var b strings.Builder
	chunk := map[string]interface{}{
		"id":     "chatcmpl_1",
		"object": "chat.completion.chunk",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": 0,
					"id":    "call_1",
					"type":  "function",
					"function": map[string]interface{}{
						"name":      "get_weather",
						"arguments": `{"city":"Shanghai"}`,
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
	}
	raw, _ := json.Marshal(chunk)
	b.WriteString("data: ")
	b.Write(raw)
	b.WriteString("\n\n")
	b.WriteString("data: [DONE]\n\n")

	rec := httptest.NewRecorder()
	WriteStreamFromChatReader(rec, CreateRequest{Model: "grok-4.20-0309"}, strings.NewReader(b.String()), StreamOptions{})

	out := rec.Body.String()
	testutil.MustContainAll(t, out, "response.output_item.added", "response.function_call_arguments.done")
	testutil.MustContainAll(t, out, `"call_id":"call_1"`, `"name":"get_weather"`)
	testutil.MustContain(t, out, `data: [DONE]`)
}

func TestWriteResponsesStreamFromChatFailsEmptyAndPrematureStreams(t *testing.T) {
	for name, input := range map[string]string{
		"empty_done": "data: [DONE]\n\n",
		"premature":  "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n",
		"error":      "data: {\"error\":{\"code\":\"rate_limit\",\"message\":\"slow down\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			WriteStreamFromChatReader(recorder, CreateRequest{Model: "grok-4.6"}, strings.NewReader(input), StreamOptions{})
			body := recorder.Body.String()
			testutil.Falsef(t, !strings.Contains(body, "event: response.failed") || strings.Contains(body, "event: response.completed"), "body=%s", body)
			testutil.MustContainAll(t, body, `"model":"grok-4.6"`, "data: [DONE]")
		})
	}
}

func TestResponsesObjectFromChatPreservesReasoningItem(t *testing.T) {
	chat := map[string]interface{}{
		"model": "grok-4.3",
		"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{
			"role": "assistant", "reasoning_content": "plan", "content": "answer",
		}}},
	}
	output := ObjectFromChat("grok-4.3", chat)["output"].([]interface{})
	testutil.Equal(t, len(output), 2)
	reasoning := output[0].(map[string]interface{})
	testutil.Equal(t, reasoning["type"], "reasoning")
	summary := reasoning["summary"].([]interface{})[0].(map[string]interface{})
	testutil.Equal(t, summary["text"], "plan")
}

func TestWriteResponsesStreamFromChatPreservesReasoningEvents(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"plan"},"finish_reason":null}]}`,
		`data: {"choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`,
		`data: [DONE]`,
	}, "\n\n")
	recorder := httptest.NewRecorder()
	WriteStreamFromChatReader(recorder, CreateRequest{Model: "grok-4.3"}, strings.NewReader(raw), StreamOptions{})
	out := recorder.Body.String()
	testutil.MustContainAll(t, out, `"type":"response.reasoning_summary_text.delta"`, `"delta":"plan"`)
	testutil.MustContainAll(t, out, `"type":"response.output_text.delta"`, `"delta":"answer"`)
}

func TestResponsesStreamTranslationIsIncremental(t *testing.T) {
	reader, writer := io.Pipe()
	recorder := newObservedStreamWriter("response.output_text.delta")
	done := make(chan struct{})
	go func() {
		WriteStreamFromChatReader(recorder, CreateRequest{Model: "grok-4.6"}, reader, StreamOptions{})
		close(done)
	}()

	_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
	select {
	case <-recorder.observed:
	case <-time.After(time.Second):
		t.Fatal("first Responses delta was buffered until stream completion")
	}
	_ = writer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("translator did not finish")
	}
}

func TestResponsesStreamAggregatesFragmentedToolArguments(t *testing.T) {
	raw := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"x\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	WriteStreamFromChatReader(recorder, CreateRequest{Model: "grok-4.6"}, strings.NewReader(raw), StreamOptions{})
	body := recorder.Body.String()
	testutil.Equal(t, strings.Count(body, "event: response.output_item.added"), 1)
	testutil.MustContain(t, body, `"arguments":"{\"x\":1}"`)
}

type observedStreamWriter struct {
	header   http.Header
	mu       sync.Mutex
	body     bytes.Buffer
	needle   string
	observed chan struct{}
	once     sync.Once
}

func newObservedStreamWriter(needle string) *observedStreamWriter {
	return &observedStreamWriter{header: make(http.Header), needle: needle, observed: make(chan struct{})}
}

func (w *observedStreamWriter) Header() http.Header { return w.header }
func (w *observedStreamWriter) WriteHeader(int)     {}
func (w *observedStreamWriter) Flush()              {}
func (w *observedStreamWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.body.Write(data)
	if strings.Contains(w.body.String(), w.needle) {
		w.once.Do(func() { close(w.observed) })
	}
	return n, err
}

func remainingChatFrame(delta map[string]interface{}, finish interface{}) string {
	b, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": delta, "finish_reason": finish}}})
	return "data: " + string(b) + "\n\n"
}

func remainingResponse(t *testing.T, stream string) map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	WriteStreamFromChatReader(w, CreateRequest{Model: "grok-4.6"}, strings.NewReader(stream), StreamOptions{})
	var final map[string]interface{}
	err := ReadSSE(strings.NewReader(w.Body.String()), func(kind, data string) error {
		if kind == "response.completed" || kind == "response.failed" || kind == "response.incomplete" {
			var event map[string]interface{}
			_ = json.Unmarshal([]byte(data), &event)
			final, _ = event["response"].(map[string]interface{})
		}
		return nil
	})
	testutil.Falsef(t, err != nil || final == nil, "%v %s", err, w.Body.String())
	return final
}

func TestRemainingResponsesRejectsBadToolAndPreservesIncomplete(t *testing.T) {
	tool := func(args string) map[string]interface{} {
		return map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "id": "call_a", "function": map[string]interface{}{"name": "Read", "arguments": args}}}}
	}
	for _, finish := range []string{"stop", "length", "content_filter"} {
		v := remainingResponse(t, remainingChatFrame(tool("{"), finish)+"data: [DONE]\n\n")
		want := "incomplete"
		if finish == "stop" {
			want = "failed"
		}
		testutil.EqualAny(t, v["status"], want)
	}
	for _, s := range []string{
		remainingChatFrame(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "function": map[string]interface{}{"arguments": "{}"}}}}, "tool_calls"),
		remainingChatFrame(tool("{}"), nil) + remainingChatFrame(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "id": "changed"}}}, "tool_calls"),
		remainingChatFrame(map[string]interface{}{"content": "a"}, "stop") + remainingChatFrame(map[string]interface{}{"content": "b"}, nil),
	} {
		v := remainingResponse(t, s+"data: [DONE]\n\n")
		testutil.Equal(t, v["status"], "failed")
	}
}

func TestRemainingResponsesLength(t *testing.T) {
	v := remainingResponse(t, remainingChatFrame(map[string]interface{}{"content": "partial"}, nil)+remainingChatFrame(map[string]interface{}{}, "length")+"data: [DONE]\n\n")
	testutil.Equal(t, v["status"], "incomplete")
}

func TestRemainingResponsesReasoningIdentity(t *testing.T) {
	s := remainingChatFrame(map[string]interface{}{"reasoning_content": "first ", "reasoning_item_id": "rs_1"}, nil) + remainingChatFrame(map[string]interface{}{"content": "A"}, nil) + remainingChatFrame(map[string]interface{}{"reasoning_content": "second", "reasoning_item_id": "rs_2"}, nil) + remainingChatFrame(map[string]interface{}{}, "stop") + "data: [DONE]\n\n"
	v := remainingResponse(t, s)
	ids := map[string]bool{}
	for _, raw := range InterfaceSlice(v["output"]) {
		item, _ := raw.(map[string]interface{})
		if item["type"] != "reasoning" {
			continue
		}
		id := chatwire.ParseLooseStringAny(item["id"])
		if ids[id] {
			t.Fatalf("reused reasoning id=%s; second summary=%v", id, item["summary"])
		}
		ids[id] = true
	}
}

func TestRemainingResponsesMalformedFrame(t *testing.T) {
	v := remainingResponse(t, remainingChatFrame(map[string]interface{}{"content": "partial"}, nil)+"data: {broken\n\n"+"data: [DONE]\n\n")
	testutil.Equal(t, v["status"], "failed")
}

type remainingBrokenResponseWriter struct{ reads int }

func (*remainingBrokenResponseWriter) Header() http.Header        { return http.Header{} }
func (*remainingBrokenResponseWriter) WriteHeader(int)            {}
func (*remainingBrokenResponseWriter) Write([]byte) (int, error)  { return 0, io.ErrClosedPipe }
func (r *remainingBrokenResponseWriter) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestRemainingResponsesWriteFailureStopsReading(t *testing.T) {
	w := &remainingBrokenResponseWriter{}
	WriteStreamFromChatReader(w, CreateRequest{Model: "grok-4.6"}, w, StreamOptions{})
	testutil.Equal(t, w.reads, 0)
}
