package grok

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"orchids-api/internal/audit"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func parityFrame(kind string, fields map[string]interface{}) string {
	if fields == nil {
		fields = map[string]interface{}{}
	}
	fields["type"] = kind
	raw, _ := json.Marshal(fields)
	return "event: " + kind + "\ndata: " + string(raw) + "\n\n"
}
func parityItem(kind, id, call, name, args string) string {
	return parityFrame(kind, map[string]interface{}{"item": map[string]interface{}{"type": "function_call", "id": id, "call_id": call, "name": name, "arguments": args}})
}
func parityTerminal(kind string) string {
	return parityFrame(kind, map[string]interface{}{"response": map[string]interface{}{"status": strings.TrimPrefix(kind, "response.")}})
}
func parityText(text string) string {
	return parityFrame("response.output_text.delta", map[string]interface{}{"delta": text})
}
func parityRun(t *testing.T, stream string, stop ...string) (string, chatOutcome) {
	t.Helper()
	rec := httptest.NewRecorder()
	result := (&Handler{}).streamBuildChatHolding(rec, &chatwire.Request{Model: "grok-4.6", Stream: true, Stop: stop}, strings.NewReader(stream), nil)
	return rec.Body.String(), result
}

type parityTool struct {
	id, name, args string
	starts         int
}

func parityTools(t *testing.T, stream string) map[int]*parityTool {
	t.Helper()
	calls := map[int]*parityTool{}
	err := responses.ReadSSE(strings.NewReader(stream), func(_, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return err
		}
		for _, raw := range responses.InterfaceSlice(chunk["choices"]) {
			choice, _ := raw.(map[string]interface{})
			delta, _ := choice["delta"].(map[string]interface{})
			for _, raw := range responses.InterfaceSlice(delta["tool_calls"]) {
				call, _ := raw.(map[string]interface{})
				index := responses.InterfaceToInt(call["index"])
				fn, _ := call["function"].(map[string]interface{})
				if calls[index] == nil {
					calls[index] = &parityTool{}
				}
				tool := calls[index]
				if id := chatwire.ParseLooseStringAny(call["id"]); id != "" {
					tool.id = id
					tool.starts++
				}
				tool.name += responses.StreamString(fn["name"])
				tool.args += responses.StreamString(fn["arguments"])
			}
		}
		return nil
	})
	testutil.NoError(t, err)
	return calls
}

func TestParityToolsDeduplicateAndAssociateInterleavedArguments(t *testing.T) {
	stream := parityItem("response.output_item.added", "fc_a", "call_a", "Read", "") + parityItem("response.output_item.added", "fc_b", "call_b", "Search", "")
	stream += parityFrame("response.function_call_arguments.delta", map[string]interface{}{"item_id": "fc_a", "delta": "{\"path\":\"hello "})
	stream += parityFrame("response.function_call_arguments.delta", map[string]interface{}{"item_id": "fc_b", "delta": "{\"query\":\"b\"}"})
	stream += parityFrame("response.function_call_arguments.delta", map[string]interface{}{"item_id": "fc_a", "delta": " world\"}"})
	stream += parityItem("response.output_item.done", "fc_a", "call_a", "Read", `{"path":"hello  world"}`) + parityItem("response.output_item.done", "fc_b", "call_b", "Search", `{"query":"b"}`) + parityTerminal("response.completed")
	body, result := parityRun(t, stream)
	testutil.Fail(t, result.Err != nil, result.Err, body)
	calls := parityTools(t, body)
	testutil.Falsef(t, len(calls) != 2 || calls[0].starts != 1 || calls[1].starts != 1 || calls[0].args != `{"path":"hello  world"}` || calls[1].args != `{"query":"b"}`, "calls=%+v body=%s", calls, body)
	var messages bytes.Buffer
	testutil.NoError(t, translateOpenAIChatStreamToAnthropicWithInput(&messages, strings.NewReader(body), "grok-4.6", 0))
	testutil.Fail(t, strings.Count(messages.String(), `"type":"tool_use"`) != 2 || strings.Contains(messages.String(), "<nil>"), messages.String())
}

func TestParityRejectsUnknownToolEventsAndInvalidIdentity(t *testing.T) {
	for name, stream := range map[string]string{
		"unknown_item":         parityFrame("response.function_call_arguments.delta", map[string]interface{}{"item_id": "unknown", "delta": "{}"}),
		"empty_name":           parityItem("response.output_item.added", "fc_a", "call_a", "", "{}"),
		"duplicate_call_id":    parityItem("response.output_item.added", "fc_a", "call_a", "Read", "{}") + parityItem("response.output_item.added", "fc_b", "call_a", "Read", "{}"),
		"invalid_json":         parityItem("response.output_item.added", "fc_a", "call_a", "Read", "{") + parityTerminal("response.completed"),
		"conflicting_snapshot": parityItem("response.output_item.added", "fc_a", "call_a", "Read", "{}") + parityItem("response.output_item.done", "fc_a", "call_a", "Read", `{"a":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			body, result := parityRun(t, stream)
			testutil.Fail(t, result.Err == nil || strings.Contains(body, `"finish_reason":"stop"`), body)
		})
	}
}

func TestParityTerminalErrorsReachMessages(t *testing.T) {
	for name, stream := range map[string]string{
		"failed":                parityFrame("response.failed", map[string]interface{}{"response": map[string]interface{}{"error": map[string]interface{}{"message": "synthetic failure"}}}),
		"empty_completed":       parityTerminal("response.completed"),
		"thinking_only":         parityFrame("response.reasoning_text.delta", map[string]interface{}{"delta": "plan"}) + parityTerminal("response.completed"),
		"eof":                   parityText("partial"),
		"done_without_terminal": "data: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			body, result := parityRun(t, stream)
			testutil.Fail(t, result.Err == nil, body)
			var out bytes.Buffer
			err := translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(body), "grok-4.6", 0)
			testutil.Fail(t, err == nil || !strings.Contains(out.String(), "event: error") || strings.Contains(out.String(), "event: message_stop"), err, out.String())
		})
	}
	for _, data := range []string{"data: {bad-json}\n\n", "data: {\"error\":{\"message\":\"explicit failure\"}}\n\n"} {
		var out bytes.Buffer
		if err := translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(data), "grok-4.6", 0); err == nil || strings.Contains(out.String(), "event: message_stop") {
			t.Fatal(out.String())
		}
	}
}

func TestParityIncompleteAndStopSequences(t *testing.T) {
	body, result := parityRun(t, parityText("partial")+parityTerminal("response.incomplete"))
	testutil.Fail(t, result.Err != nil || result.Finish != "length", result, body)
	var out bytes.Buffer
	if err := translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(body), "grok-4.6", 0); err != nil || !strings.Contains(out.String(), `"stop_reason":"max_tokens"`) {
		t.Fatal(err, out.String())
	}
	for _, prefix := range []string{"before ", ""} {
		body, result = parityRun(t, parityText(prefix+"E")+parityText("N")+parityText("D after")+parityTerminal("response.completed"), "END")
		testutil.Fail(t, result.Err != nil || strings.Contains(body, "after") || !strings.Contains(body, `"stop_sequence":"END"`), result, body)
		out.Reset()
		if err := translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(body), "grok-4.6", 0); err != nil || !strings.Contains(out.String(), `"stop_reason":"stop_sequence"`) {
			t.Fatal(err, out.String())
		}
	}
	filter := stopFilter{sequences: []string{"结束"}}
	text := filter.push("正文结", false) + filter.push("束后缀", false) + filter.push("", true)
	testutil.Fail(t, text != "正文" || filter.matched != "结束", text, filter)
}

func TestParityMissingToolIDsAndUsage(t *testing.T) {
	for _, id := range []interface{}{nil, "", 42, "<nil>"} {
		_, err := anthropicMessageToChat(anthropicMessage{Role: "assistant", Content: []interface{}{map[string]interface{}{"type": "tool_use", "id": id, "name": "Read", "input": map[string]interface{}{}}}})
		testutil.Fail(t, err == nil || strings.Contains(err.Error(), "duplicate"), id, err)
	}
	for _, cached := range []int{0, 80, 180} {
		usage := anthropicUsageFromOpenAI(map[string]interface{}{"prompt_tokens": 100, "completion_tokens": 10, "prompt_tokens_details": map[string]interface{}{"cached_tokens": cached}})
		testutil.Equal(t, responses.InterfaceToInt(usage["input_tokens"])+responses.InterfaceToInt(usage["cache_read_input_tokens"]), 100)
	}
}

type parityNoticeWriter struct {
	*httptest.ResponseRecorder
	notice chan struct{}
	once   bool
}

func (w *parityNoticeWriter) Write(data []byte) (int, error) {
	if !w.once && bytes.Contains(data, []byte(`"name":"Read"`)) {
		w.once = true
		close(w.notice)
	}
	return w.ResponseRecorder.Write(data)
}
func TestParityToolsAreVisibleBeforeStreamCompletes(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	w := &parityNoticeWriter{ResponseRecorder: httptest.NewRecorder(), notice: make(chan struct{})}
	done := make(chan chatOutcome, 1)
	go func() {
		done <- (&Handler{}).streamBuildChatHolding(w, &chatwire.Request{Model: "grok-4.6"}, reader, nil)
	}()
	_, _ = io.WriteString(writer, parityItem("response.output_item.added", "fc_a", "call_a", "Read", ""))
	select {
	case <-w.notice:
	case <-time.After(time.Second):
		t.Fatal("tool identity buffered until EOF")
	}
	_, _ = io.WriteString(writer, parityItem("response.output_item.done", "fc_a", "call_a", "Read", "{}")+parityTerminal("response.completed"))
	select {
	case result := <-done:
		testutil.Fail(t, result.Err != nil, result.Err)
	case <-time.After(time.Second):
		t.Fatal("terminal event did not complete the response")
	}
}

func TestParityReasoningAndSearchBlocks(t *testing.T) {
	stream := ""
	for _, id := range []string{"rs1", "rs2"} {
		stream += parityFrame("response.output_item.added", map[string]interface{}{"item": map[string]interface{}{"type": "reasoning", "id": id}})
		stream += parityFrame("response.reasoning_summary_text.delta", map[string]interface{}{"item_id": id, "delta": "plan " + id})
		stream += parityFrame("response.reasoning_text.delta", map[string]interface{}{"item_id": id, "delta": "duplicate " + id})
		stream += parityFrame("response.output_item.done", map[string]interface{}{"item": map[string]interface{}{"type": "reasoning", "id": id, "encrypted_content": "signature_" + id}})
	}
	search := map[string]interface{}{"type": "web_search_call", "id": "ws1", "status": "completed", "action": map[string]interface{}{"query": "current news", "sources": []interface{}{map[string]interface{}{"url": "https://example.com/news", "title": "News"}}}}
	stream += parityFrame("response.output_item.added", map[string]interface{}{"item": search}) + parityFrame("response.output_item.done", map[string]interface{}{"item": search})
	stream += parityText("answer") + parityFrame("response.output_text.annotation.added", map[string]interface{}{"annotation": map[string]interface{}{"type": "url_citation", "url": "https://example.com/news", "title": "News"}}) + parityTerminal("response.completed")
	body, result := parityRun(t, stream)
	testutil.Fail(t, result.Err != nil, result.Err)
	var out bytes.Buffer
	testutil.NoError(t, translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(body), "grok-4.6", 0))
	s := out.String()
	testutil.Fail(t, strings.Count(s, `"content_block":{"signature":"","thinking":"","type":"thinking"}`) != 2 || strings.Contains(s, "duplicate") || strings.Count(s, `"type":"server_tool_use"`) != 1 || strings.Count(s, `"type":"web_search_tool_result"`) != 1 || !strings.Contains(s, `"type":"citations_delta"`), s)
}

func TestParityClientSearchToolsRemainClientTools(t *testing.T) {
	h := &Handler{}
	spec, _ := ResolveModel("grok-4.6")
	request := &chatwire.Request{Messages: []chatwire.Message{{Role: "user", Content: "hello"}}, Tools: []chatwire.ToolDef{{Type: "function", Function: map[string]interface{}{"name": "web_search", "parameters": map[string]interface{}{"type": "object"}}}}}
	payload, err := h.responsesPayloadFromChat(spec, request, false)
	testutil.NoError(t, err)
	tools := responses.InterfaceMaps(payload["tools"])
	testutil.Fail(t, len(tools) != 1 || tools[0]["type"] != "function" || tools[0]["parameters"] == nil, tools)
	request.Tools = nil
	payload, err = h.responsesPayloadFromChat(spec, request, false)
	testutil.Fail(t, err != nil || len(responses.InterfaceMaps(payload["tools"])) != 0, payload, err)
}

type parityAuditLog struct{ events []audit.Event }

func (l *parityAuditLog) Log(_ context.Context, event audit.Event) {
	l.events = append(l.events, event)
}
func TestParityAuditCapturesTerminalUsageAndFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		var events parityAuditLog
		h := &Handler{auditLogger: &events}
		kind := "response.completed"
		response := map[string]interface{}{"status": "completed", "usage": map[string]interface{}{"input_tokens": 100, "output_tokens": 10, "input_tokens_details": map[string]interface{}{"cached_tokens": 80}}}
		if failed {
			kind = "response.failed"
			response["error"] = map[string]interface{}{"message": "failed upstream"}
		}
		_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader(parityText("hello")+parityFrame(kind, map[string]interface{}{"response": response})), "text/event-stream", "grok-4.6")
		h.auditChatOutcome(context.Background(), &store.Account{ID: 1}, &chatwire.Request{Model: "grok-4.6", StartedAt: time.Now().Add(-time.Second)}, result)
		testutil.Fail(t, len(events.events) != 1 || events.events[0].InputTokens != 100 || events.events[0].CachedInputTokens != 80 || (events.events[0].Status == "error") != failed, events.events)
	}
}

func TestParityNonStreamingDoesNotLeakReasoningAndPreservesAllMessages(t *testing.T) {
	for _, onlyReasoning := range []bool{false, true} {
		output := []interface{}{map[string]interface{}{"type": "reasoning", "content": []interface{}{map[string]interface{}{"type": "reasoning_text", "text": "private plan"}}}}
		if !onlyReasoning {
			for _, text := range []string{"first ", " second"} {
				output = append(output, map[string]interface{}{"type": "message", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": text}}})
			}
		}
		data, _ := json.Marshal(map[string]interface{}{"status": "completed", "output": output})
		rec := httptest.NewRecorder()
		result := (&Handler{}).collectBuildChat(rec, &chatwire.Request{Model: "grok-4.6"}, bytes.NewReader(data))
		if onlyReasoning {
			testutil.False(t, result.Err == nil, "reasoning-only completion accepted")
		} else if result.Err != nil || !strings.Contains(rec.Body.String(), `"content":"first  second"`) {
			t.Fatal(result, rec.Body.String())
		}
	}
}

type parityFailedWriter struct{}

func (*parityFailedWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
func TestParityMessagesPropagatesWriteFailure(t *testing.T) {
	err := translateOpenAIChatStreamToAnthropicWithInput(&parityFailedWriter{}, strings.NewReader("data: [DONE]\n\n"), "grok-4.6", 0)
	testutil.Fail(t, err == nil || !strings.Contains(err.Error(), "client disconnected"), err)
}
