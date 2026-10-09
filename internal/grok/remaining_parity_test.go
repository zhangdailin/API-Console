package grok

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"

	"encoding/json"
)

func TestRemainingSearchRestrictionsValidationAndWire(t *testing.T) {
	for _, test := range []struct {
		tool anthropicTool
		bad  bool
	}{
		{anthropicTool{AllowedDomains: []string{"example.com"}}, false},
		{anthropicTool{BlockedDomains: []string{"example.com"}}, false},
		{anthropicTool{AllowedDomains: []string{"a"}, BlockedDomains: []string{"b"}}, true},
		{anthropicTool{BlockedDomains: []string{"a"}, ExcludedDomains: []string{"b"}}, true},
		{anthropicTool{AllowedDomains: []string{" "}}, true},
		{anthropicTool{AllowedDomains: []string{"a", "b", "c", "d", "e", "f"}}, true},
	} {
		tool, err := anthropicSearchTool(test.tool)
		testutil.Falsef(t, (err != nil) != test.bad, "tool=%v err=%v", test.tool, err)
		if err == nil && len(test.tool.BlockedDomains) > 0 {
			filters := tool["filters"].(map[string]interface{})
			testutil.Fail(t, filters["excluded_domains"] == nil, tool)
		}
	}
	var req anthropicMessagesRequest
	err := json.Unmarshal([]byte(`{"tools":[{"allowed_domains":[1]}]}`), &req)
	testutil.Error(t, err)
	tool, _ := anthropicSearchTool(anthropicTool{AllowedDomains: []string{"example.com"}})
	h := &Handler{}
	for _, build := range []bool{false, true} {
		payload, err := h.responsesPayloadFromChat(ModelSpec{ID: "grok-4.6", UpstreamModel: "grok-4.6"}, &chatwire.Request{Model: "grok-4.6", Messages: []chatwire.Message{{Role: "user", Content: "search"}}, ResponsesTools: []map[string]interface{}{tool}}, build)
		testutil.NoError(t, err)
		data, _ := json.Marshal(payload)
		testutil.MustContain(t, string(data), "example.com")
	}
}

func TestRemainingRefusalNonstream(t *testing.T) {
	raw := `{"status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"Cannot help."}]}]}`
	w := httptest.NewRecorder()
	out := (&Handler{}).collectBuildChat(w, &chatwire.Request{Model: "grok-4.6"}, strings.NewReader(raw))
	testutil.Fail(t, out.Err != nil, out.Err)
	var chat map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &chat)
	for _, v := range []map[string]interface{}{anthropicResponseFromChat("grok-4.6", chat), responsesObjectFromChat("grok-4.6", chat)} {
		b, _ := json.Marshal(v)
		testutil.MustContain(t, string(b), "Cannot help.")
	}

}

func TestRemainingResponsesSearchCitationsAndUsage(t *testing.T) {
	search := map[string]interface{}{"type": "web_search_call", "id": "ws_a", "status": "completed", "action": map[string]interface{}{"type": "search", "query": "news", "sources": []interface{}{map[string]interface{}{"url": "https://example.com", "title": "source"}}}}
	ann := map[string]interface{}{"type": "url_citation", "url_citation": map[string]interface{}{"url": "https://example.com", "title": "source", "start_index": 0, "end_index": 6}}
	s := remainingChatFrame(map[string]interface{}{"x_grok_search": search, "x_grok_search_done": false}, nil) + remainingChatFrame(map[string]interface{}{"x_grok_search": search, "x_grok_search_done": true}, nil) + remainingChatFrame(map[string]interface{}{"content": "answer"}, nil) + remainingChatFrame(map[string]interface{}{"annotations": []interface{}{ann, ann}}, "stop") + "data: [DONE]\n\n"
	v := remainingResponse(t, s)
	items := interfaceSlice(v["output"])
	testutil.Equal(t, len(items), 2)
	first := items[0].(map[string]interface{})
	testutil.Fail(t, first["type"] != "web_search_call" || first["status"] != "completed", first)
	msg := items[1].(map[string]interface{})
	part := interfaceSlice(msg["content"])[0].(map[string]interface{})
	annotations := interfaceSlice(part["annotations"])
	testutil.Fail(t, len(annotations) != 1 || annotations[0].(map[string]interface{})["url"] != "https://example.com", annotations)
	chat := map[string]interface{}{"choices": []interface{}{map[string]interface{}{"message": map[string]interface{}{"content": " answer ", "annotations": []interface{}{ann}, "x_grok_searches": []interface{}{search}}, "finish_reason": "length"}}, "usage": map[string]interface{}{"prompt_tokens": 10, "completion_tokens": 4, "prompt_tokens_details": map[string]interface{}{"cached_tokens": 3}, "completion_tokens_details": map[string]interface{}{"reasoning_tokens": 2}}}
	v = responsesObjectFromChat("grok-4.6", chat)
	testutil.Fail(t, v["status"] != "incomplete" || v["incomplete_details"] == nil, v)
	b, _ := json.Marshal(v)
	testutil.MustContainAll(t, string(b), `"text":" answer "`, `"cached_tokens":3`, `"reasoning_tokens":2`)
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

func TestRemainingEarlyMessagesSignature(t *testing.T) {
	s := parityFrame("response.output_item.added", map[string]interface{}{"item": map[string]interface{}{"type": "reasoning", "id": "rs_a", "encrypted_content": "synthetic-signature"}}) + parityFrame("response.reasoning_text.delta", map[string]interface{}{"item_id": "rs_a", "delta": "plan"}) + parityText("answer") + parityTerminal("response.completed")
	body, out := parityRun(t, s)
	testutil.Fail(t, out.Err != nil, out.Err)
	var messages bytes.Buffer
	err := translateOpenAIChatStreamToAnthropicWithInput(&messages, strings.NewReader(body), "grok-4.6", 0)
	testutil.NoError(t, err)
	testutil.MustContain(t, messages.String(), "synthetic-signature")
}

func remainingChatFrame(delta map[string]interface{}, finish interface{}) string {
	b, _ := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"delta": delta, "finish_reason": finish}}})
	return "data: " + string(b) + "\n\n"
}
func remainingResponse(t *testing.T, stream string) map[string]interface{} {
	t.Helper()
	w := httptest.NewRecorder()
	writeResponsesStreamFromChatReaderRequestWithHook(w, responses.CreateRequest{Model: "grok-4.6"}, strings.NewReader(stream), nil)
	var final map[string]interface{}
	err := responses.ReadSSE(strings.NewReader(w.Body.String()), func(kind, data string) error {
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
func TestRemainingTerminalSignature(t *testing.T) {
	s := parityFrame("response.reasoning_text.delta", map[string]interface{}{"item_id": "rs_a", "delta": "plan"}) + parityText("answer") + parityFrame("response.completed", map[string]interface{}{"response": map[string]interface{}{"status": "completed", "output": []interface{}{map[string]interface{}{"type": "reasoning", "id": "rs_a", "encrypted_content": "synthetic-signature"}}}})
	body, out := parityRun(t, s)
	testutil.Fail(t, out.Err != nil, out.Err)
	testutil.MustContain(t, body, "synthetic-signature")
}
func TestRemainingRefusal(t *testing.T) {
	body, out := parityRun(t, parityFrame("response.refusal.delta", map[string]interface{}{"delta": "I cannot help with that."})+parityTerminal("response.completed"))
	testutil.Falsef(t, out.Err != nil || !strings.Contains(body, "I cannot help with that."), "refusal lost; err=%v", out.Err)
}
func TestRemainingResponsesLength(t *testing.T) {
	v := remainingResponse(t, remainingChatFrame(map[string]interface{}{"content": "partial"}, nil)+remainingChatFrame(map[string]interface{}{}, "length")+"data: [DONE]\n\n")
	testutil.Equal(t, v["status"], "incomplete")
}
func TestRemainingResponsesReasoningIdentity(t *testing.T) {
	s := remainingChatFrame(map[string]interface{}{"reasoning_content": "first ", "reasoning_item_id": "rs_1"}, nil) + remainingChatFrame(map[string]interface{}{"content": "A"}, nil) + remainingChatFrame(map[string]interface{}{"reasoning_content": "second", "reasoning_item_id": "rs_2"}, nil) + remainingChatFrame(map[string]interface{}{}, "stop") + "data: [DONE]\n\n"
	v := remainingResponse(t, s)
	ids := map[string]bool{}
	for _, raw := range interfaceSlice(v["output"]) {
		item, _ := raw.(map[string]interface{})
		if item["type"] != "reasoning" {
			continue
		}
		id := interfaceString(item["id"])
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
func TestRemainingSearchDomainRestriction(t *testing.T) {
	var req anthropicMessagesRequest
	_ = json.Unmarshal([]byte(`{"model":"grok-4.6","max_tokens":100,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"]}]}`), &req)
	out, err := anthropicRequestToChat(req)
	testutil.NoError(t, err)
	b, _ := json.Marshal(out.ResponsesTools)
	testutil.MustContain(t, string(b), "example.com")
}

func TestRemainingResponsesUsageDetails(t *testing.T) {
	out := responsesUsageFromChat(map[string]interface{}{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "prompt_tokens_details": map[string]interface{}{"cached_tokens": 80}, "completion_tokens_details": map[string]interface{}{"reasoning_tokens": 10}})
	testutil.Falsef(t, out["input_tokens_details"] == nil || out["output_tokens_details"] == nil, "detailed usage lost: %v", out)
}
func TestRemainingTerminalCitations(t *testing.T) {
	s := parityText("answer") + parityFrame("response.completed", map[string]interface{}{"response": map[string]interface{}{"status": "completed", "output": []interface{}{map[string]interface{}{"type": "message", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "answer", "annotations": []interface{}{map[string]interface{}{"type": "url_citation", "url": "https://example.com/source", "title": "Source", "start_index": 0, "end_index": 6}}}}}}}})
	body, out := parityRun(t, s)
	testutil.Fail(t, out.Err != nil, out.Err)
	testutil.MustContain(t, body, "https://example.com/source")
}

func TestRemainingLateMessagesSignatureKeepsOriginalBlock(t *testing.T) {
	s := parityFrame("response.reasoning_text.delta", map[string]interface{}{"item_id": "rs_a", "delta": "plan"}) + parityText("answer") + parityFrame("response.completed", map[string]interface{}{"response": map[string]interface{}{"status": "completed", "output": []interface{}{map[string]interface{}{"type": "reasoning", "id": "rs_a", "encrypted_content": "late-signature"}}}})
	chat, out := parityRun(t, s)
	testutil.Fail(t, out.Err != nil, out.Err)
	var messages bytes.Buffer
	testutil.NoError(t, translateOpenAIChatStreamToAnthropicWithInput(&messages, strings.NewReader(chat), "grok-4.6", 0))
	thinkingIndex := -1
	count := 0
	signed := false
	open := map[int]bool{}
	err := responses.ReadSSE(strings.NewReader(messages.String()), func(kind, data string) error {
		var v map[string]interface{}
		_ = json.Unmarshal([]byte(data), &v)
		index := interfaceToInt(v["index"])
		switch kind {
		case "content_block_start":
			open[index] = true
			block, _ := v["content_block"].(map[string]interface{})
			if block["type"] == "thinking" {
				count++
				thinkingIndex = index
			}
		case "content_block_stop":
			delete(open, index)
		case "content_block_delta":
			testutil.CheckFalse(t, !open[index], "delta after closed block")
			delta, _ := v["delta"].(map[string]interface{})
			if delta["type"] == "signature_delta" {
				signed = true
				testutil.CheckFalse(t, index != thinkingIndex || delta["signature"] != "late-signature", "signature detached from thinking")
			}
		}
		return nil
	})
	testutil.Fail(t, err != nil || count != 1 || !signed || len(open) != 0, err, count, signed, len(open))
}

type remainingBrokenResponseWriter struct{ reads int }

func (*remainingBrokenResponseWriter) Header() http.Header        { return http.Header{} }
func (*remainingBrokenResponseWriter) WriteHeader(int)            {}
func (*remainingBrokenResponseWriter) Write([]byte) (int, error)  { return 0, io.ErrClosedPipe }
func (r *remainingBrokenResponseWriter) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }

func TestRemainingResponsesWriteFailureStopsReading(t *testing.T) {
	w := &remainingBrokenResponseWriter{}
	writeResponsesStreamFromChatReaderRequestWithHook(w, responses.CreateRequest{Model: "grok-4.6"}, w, nil)
	testutil.Equal(t, w.reads, 0)
}
