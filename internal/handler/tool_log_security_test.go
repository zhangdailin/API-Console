package handler

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestRejectedToolsNeverLogArgumentsOrUntrustedNames(t *testing.T) {
	var buffer bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buffer, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prior) })
	for _, tc := range []struct {
		gate        bool
		name, input string
	}{
		{true, "secret-tool-name", `{"password":"secret-input-value"}`},
		{false, "secret-tool-name", `{"password":"secret-input-value"}`},
		{false, "declared", `["secret-input-value"]`},
	} {
		buffer.Reset()
		h := &streamHandler{config: &config.Config{DebugEnabled: true}, allowedToolNames: map[string]struct{}{"declared": {}}, disallowToolCalls: tc.gate}
		if h.shouldAcceptToolCall(toolCall{name: tc.name, input: tc.input}) {
			t.Fatal("rejected fixture was accepted")
		}
		if strings.Contains(buffer.String(), "secret-") {
			t.Fatal("rejected tool secrets reached debug log")
		}
		if !strings.Contains(buffer.String(), "input_bytes=") {
			t.Fatal("safe diagnostic metadata missing")
		}
	}
}
