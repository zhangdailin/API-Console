package warp

import (
	"orchids-api/internal/prompt"
	"strings"
	"testing"
)

func TestToolContinuationKeepsFreshInstructionText(t *testing.T) {
	req := testRequest()
	req.Tools = []any{map[string]any{"name": "echo", "description": "echo", "input_schema": map[string]any{"type": "object"}}}
	req.System = []prompt.SystemItem{{Type: "text", Text: "base instruction"}}
	req.Messages = []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "echo it"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call1", Name: "echo", Input: map[string]any{}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call1", Content: "ok"}}}},
		{Role: "system", Content: prompt.MessageContent{Text: "fresh instruction"}},
	}
	p, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.ToolResults) != 1 || !strings.Contains(p.CurrentText, "base instruction") || !strings.Contains(p.CurrentText, "fresh instruction") || strings.Contains(p.CurrentText, "echo it") {
		t.Fatal("continuation lost fresh instructions or replayed dialog")
	}
}
