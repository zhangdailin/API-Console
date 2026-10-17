package handler

import (
	"orchids-api/internal/adapter"
	"orchids-api/internal/upstream"
	"strings"
	"testing"
)

func TestIncrementalToolArgumentsBeforeFinish(t *testing.T) {
	for _, format := range []adapter.ResponseFormat{adapter.FormatAnthropic, adapter.FormatOpenAI} {
		h, w := newStreamTestHandler(t, true, format)
		h.setAllowedToolNames([]string{"shell"})
		delta := func(id, args string) {
			h.handleMessage(upstream.SSEMessage{Type: "model.tool-call-delta", Event: map[string]any{"toolCallId": id, "toolName": "shell", "delta": args}})
		}
		delta("a", `{"cmd":`)
		if !strings.Contains(w.buf.String(), "shell") || !strings.Contains(w.buf.String(), "cmd") {
			t.Fatalf("missing early delta: %s", w.buf.String())
		}
		delta("b", `{"cmd":"ls"}`)
		delta("a", `"pwd"}`)
		for _, id := range []string{"a", "b"} {
			h.handleMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]any{"toolCallId": id, "toolName": "shell", "input": `{"cmd":"pwd"}`}})
		}
		if h.toolCallCount != 2 || len(h.streamingTools) != 0 {
			t.Fatalf("duplicate or unclosed tools: %d %v", h.toolCallCount, h.streamingTools)
		}
		if strings.Count(w.buf.String(), "shell") != 2 {
			t.Fatalf("tool name emitted twice: %s", w.buf.String())
		}
	}
}

func TestIncrementalToolsRespectGate(t *testing.T) {
	h, w := newStreamTestHandler(t, true, adapter.FormatAnthropic)
	h.setDisallowToolCalls(true)
	h.handleMessage(upstream.SSEMessage{Type: "model.tool-call-delta", Event: map[string]any{"toolCallId": "x", "toolName": "shell", "delta": "{}"}})
	if w.buf.Len() != 0 || h.toolCallCount != 0 {
		t.Fatal("disallowed tool leaked")
	}
}
