package warp

import (
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func systemRequest(system, user string) upstream.UpstreamRequest {
	return upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{
		{Role: "system", Content: prompt.MessageContent{Text: system}},
		{Role: "user", Content: prompt.MessageContent{Text: user}},
	}}
}

func TestValidateRequestFoldsOneBoundedSystemMessage(t *testing.T) {
	prepared, err := validateRequest(systemRequest("Answer briefly.", "hi"))
	if err != nil {
		t.Fatalf("bounded system message rejected: %v", err)
	}
	if !strings.Contains(prepared.Query, "Answer briefly.") || !strings.Contains(prepared.Query, "hi") {
		t.Fatal("system instructions were dropped")
	}
	if _, err := validateRequest(systemRequest(strings.Repeat("s", 128<<10), "hi")); err != nil {
		t.Fatalf("uncapped system message rejected: %v", err)
	}
	if _, err := validateRequest(systemRequest("ok", strings.Repeat("u", 128<<10))); err != nil {
		t.Fatal("large user message rejected")
	}
	for name, req := range map[string]upstream.UpstreamRequest{
		"assistant_turn":  {Model: "model-a", Messages: []prompt.Message{{Role: "assistant", Content: prompt.MessageContent{Text: "x"}}, {Role: "user", Content: prompt.MessageContent{Text: "hi"}}}},
		"system_only":     {Model: "model-a", Messages: []prompt.Message{{Role: "system", Content: prompt.MessageContent{Text: "a"}}}},
		"empty_system":    systemRequest("   ", "hi"),
		"assistant_first": {Model: "model-a", Messages: []prompt.Message{{Role: "assistant", Content: prompt.MessageContent{Text: "a"}}, {Role: "user", Content: prompt.MessageContent{Text: "b"}}}},
		"image_block": {Model: "model-a", Messages: []prompt.Message{
			{Role: "system", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "image_url", URL: "https://example.invalid"}}}},
			{Role: "user", Content: prompt.MessageContent{Text: "hi"}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateRequest(req); err == nil {
				t.Fatal("history or non-text content accepted")
			}
		})
	}
}
