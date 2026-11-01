package handler

import (
	"testing"

	"orchids-api/internal/adapter"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestStreamHandler_NoToolsGateSuppressesValidToolCall(t *testing.T) {
	sh, rec := newStreamTestHandler(t, true, adapter.FormatAnthropic)

	sh.setDisallowToolCalls(true)
	sh.handleMessage(upstream.SSEMessage{
		Type: "model",
		Event: map[string]any{
			"type":       "tool-call",
			"toolCallId": "tool_1",
			"toolName":   "Read",
			"input":      `{"file_path":"README.md"}`,
		},
	})
	sh.finishResponse("tool_use")

	out := rec.buf.String()
	testutil.MustNotContain(t, out, `"type":"tool_use"`)
	testutil.MustContain(t, out, `"stop_reason":"end_turn"`)
}
