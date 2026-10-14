package grok

import (
	"bytes"
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
	for _, v := range []map[string]interface{}{anthropicResponseFromChat("grok-4.6", chat)} {
		b, _ := json.Marshal(v)
		testutil.MustContain(t, string(b), "Cannot help.")
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
func TestRemainingSearchDomainRestriction(t *testing.T) {
	var req anthropicMessagesRequest
	_ = json.Unmarshal([]byte(`{"model":"grok-4.6","max_tokens":100,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"]}]}`), &req)
	out, err := anthropicRequestToChat(req)
	testutil.NoError(t, err)
	b, _ := json.Marshal(out.ResponsesTools)
	testutil.MustContain(t, string(b), "example.com")
}

func TestRemainingResponsesUsageDetails(t *testing.T) {
	out := responses.UsageFromChat(map[string]interface{}{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120, "prompt_tokens_details": map[string]interface{}{"cached_tokens": 80}, "completion_tokens_details": map[string]interface{}{"reasoning_tokens": 10}})
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
		index := responses.InterfaceToInt(v["index"])
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
