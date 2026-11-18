package warp

import (
	"encoding/json"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"orchids-api/internal/prompt"
)

func TestHistoricalThinkingIsNotReplayedAsQueryOrToolInput(t *testing.T) {
	const privateThought = "synthetic-private-reasoning"
	const privateSignature = "synthetic-provider-signature"
	raw := `{"model":"model-a","messages":[{"role":"user","content":"first"},{"role":"assistant","content":[{"type":"thinking","thinking":"` + privateThought + `","signature":"` + privateSignature + `"},{"type":"text","text":"answer"}]},{"role":"user","content":"next"}]}`
	if err := validateRawRequest(json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	req := testRequest()
	if json.Unmarshal([]byte(raw), &struct {
		Messages *[]prompt.Message `json:"messages"`
	}{Messages: &req.Messages}) != nil {
		t.Fatal("invalid fixture")
	}
	p, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.Query, "answer") || strings.Contains(p.Query, privateThought) || strings.Contains(p.Query, privateSignature) {
		t.Fatal("history reasoning forwarded or answer lost")
	}
	body, err := buildRequest("model-a", p)
	if err != nil {
		t.Fatal(err)
	}
	var request warpapi.Request
	if proto.Unmarshal(body, &request) != nil {
		t.Fatal("invalid protobuf")
	}
	query := request.GetInput().GetUserInputs().GetInputs()[0].GetUserQuery().GetQuery()
	if strings.Contains(query, privateThought) || strings.Contains(query, privateSignature) {
		t.Fatal("private reasoning escaped in query")
	}
}

func TestHistoricalThinkingToolCallAndResultRemainIntact(t *testing.T) {
	req := testRequest()
	req.Tools = []any{map[string]any{"name": "echo", "input_schema": map[string]any{"type": "object"}}}
	req.Messages = []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "first"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "thinking", Thinking: "synthetic-thought"}, {Type: "tool_use", ID: "call-1", Name: "echo", Input: map[string]any{"text": "OK"}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call-1", Content: "OK"}}}},
	}
	p, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ToolResults) != 1 || p.ToolResults[0].ID != "call-1" || p.ToolResults[0].Content != "OK" || !strings.Contains(p.Query, "Assistant tool call") || strings.Contains(p.Query, "synthetic-thought") {
		t.Fatal("tool pair changed by historical thinking conversion")
	}
}

func TestHistoricalThinkingRejectsForeignFieldsRolesAndLimits(t *testing.T) {
	bad := []string{
		`{"type":"thinking","thinking":123}`,
		`{"type":"thinking","thinking":"x","signature":null}`,
		`{"type":"thinking","thinking":"x","unknown":"private-value"}`,
		`{"type":"redacted_thinking","data":"opaque"}`,
	}
	for _, b := range bad {
		raw := `{"model":"model-a","messages":[{"role":"user","content":"first"},{"role":"assistant","content":[` + b + `,{"type":"text","text":"answer"}]},{"role":"user","content":"next"}]}`
		err := validateRawRequest(json.RawMessage(raw))
		if err == nil {
			t.Fatal("unsupported history accepted")
		}
		if strings.Contains(err.Error(), "private-value") {
			t.Fatal("history content in error")
		}
	}
	req := testRequest()
	req.Messages = []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "q"}}, {Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "thinking", Thinking: "only"}}}}, {Role: "user", Content: prompt.MessageContent{Text: "q2"}}}
	if _, err := validateRequest(req); err == nil {
		t.Fatal("thinking-only assistant treated as a visible answer")
	}
	req.Messages[1].Content.Blocks = append(req.Messages[1].Content.Blocks, prompt.ContentBlock{Type: "text", Text: "ok"})
	req.Messages[1].Content.Blocks[0].Thinking = strings.Repeat("x", thinkingHistoryLimit)
	if _, err := validateRequest(req); err != nil {
		t.Fatal("exact reasoning byte boundary rejected")
	}
	req.Messages[1].Content.Blocks[0].Signature = "x"
	if _, err := validateRequest(req); err == nil {
		t.Fatal("over-bound reasoning accepted")
	}
}
