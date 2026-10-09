package handler

import (
	"testing"

	"orchids-api/internal/adapter"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestStreamHandler_NoToolsGateSuppressesValidToolCall(t *testing.T) {
	sh, rec := newStreamTestHandler(t, false, true, adapter.FormatAnthropic)

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

func TestStreamHandler_NoToolsWriteReturnsContentAsText(t *testing.T) {
	sh, rec := newStreamTestHandler(t, false, true, adapter.FormatAnthropic)

	sh.setAllowedToolNames(nil)
	sh.setSurfaceToolRejects(true)
	sh.setDisallowToolCalls(true)
	sh.handleMessage(upstream.SSEMessage{
		Type: "model.tool-call",
		Event: map[string]any{
			"toolCallId": "tool_write_1",
			"toolName":   "Write",
			"input":      `{"file_path":"index.html","content":"<!doctype html><h1>Ready</h1>"}`,
		},
	})
	sh.finishResponse("tool_use")

	out := rec.buf.String()
	testutil.MustNotContain(t, out, `"type":"tool_use"`)
	testutil.MustContain(t, out, "Ready")
	testutil.MustContain(t, out, `"stop_reason":"end_turn"`)
}
