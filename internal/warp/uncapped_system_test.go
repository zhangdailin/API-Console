package warp

import (
	"orchids-api/internal/prompt"
	"strings"
	"testing"
)

func TestUncappedSystemDoesNotDisplaceDialog(t *testing.T) {
	system := strings.Repeat("s", 256<<10)
	messages := []prompt.Message{textMessage("user", "old"), textMessage("assistant", "answer"), textMessage("user", "now")}
	dialog, ok := renderTranscript("", messages)
	if !ok {
		t.Fatal("invalid fixture")
	}
	query, ok := renderTranscript(system, messages)
	if !ok || query != "System instructions:\n"+system+"\n\n"+dialog {
		t.Fatal("large system displaced dialog or was truncated")
	}
}

func TestUncappedSystemSurvivesToolContinuation(t *testing.T) {
	req := testRequest()
	system := strings.Repeat("s", 128<<10)
	req.System = []prompt.SystemItem{{Type: "text", Text: system}}
	req.Tools = []any{map[string]any{"name": "echo", "input_schema": map[string]any{"type": "object"}}}
	req.Messages = []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: "first"}},
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call-1", Name: "echo", Input: map[string]any{}}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call-1", Content: "OK"}}}},
	}
	p, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if p.CurrentText != "System instructions:\n"+system || !strings.HasPrefix(p.Query, "System instructions:\n"+system+"\n\n") || len(p.ToolResults) != 1 {
		t.Fatal("system lost from bound continuation or tool result")
	}
}
