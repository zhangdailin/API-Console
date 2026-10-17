package handler

import (
	"encoding/json"
	"orchids-api/internal/chatwire"
	"reflect"
	"strings"
	"testing"
)

func TestTypedChatEntryMatchesHTTPDecode(t *testing.T) {
	effort := "none"
	zero := 0.0
	c := chatwire.Request{Model: "test", Stream: true, ReasoningEffort: &effort, Temperature: &zero,
		Messages: []chatwire.Message{
			{Role: "system", Content: "system"}, {Role: "user", Content: []any{map[string]any{"type": "text", "text": "hello"}}},
			{Role: "assistant", Content: "calling", ReasoningContent: "reason", ToolCalls: []chatwire.ToolCall{{ID: "call1", Type: "function", Function: map[string]any{"name": "shell", "arguments": "{\"cmd\":\"pwd\"}"}}}},
			{Role: "tool", ToolCallID: "call1", Content: "done"}},
		Tools:        []chatwire.ToolDef{{Type: "function", Function: map[string]any{"name": "shell", "parameters": map[string]any{"type": "object"}}}},
		ResponseText: map[string]any{"verbosity": "low"}, Metadata: map[string]any{"a": "b"}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var want ClaudeRequest
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	got, err := requestFromChat(c)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("typed conversion differs:\ngot %#v\nwant %#v", got, want)
	}
}

func BenchmarkChatEntryLongHistory(b *testing.B) {
	c := chatwire.Request{Model: "test"}
	for i := 0; i < 200; i++ {
		c.Messages = append(c.Messages, chatwire.Message{Role: "user", Content: strings.Repeat("example text ", 100)})
	}
	b.Run("HTTPDecode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			raw, _ := json.Marshal(c)
			var req ClaudeRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("TypedEntry", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := json.Marshal(c); err != nil {
				b.Fatal(err)
			}
			if _, err := requestFromChat(c); err != nil {
				b.Fatal(err)
			}
		}
	})
}
