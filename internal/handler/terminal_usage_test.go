package handler

import (
	"encoding/json"
	"maps"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
)

func newTerminalUsageTestHandler(t *testing.T) (*streamHandler, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	logger := debug.New(false, false)
	sh := newStreamHandler(&config.Config{}, rec, logger, true, adapter.FormatAnthropic)
	t.Cleanup(func() { sh.release(); logger.Close() })
	return sh, rec
}

func terminalMessagesUsage(t *testing.T, body string) map[string]int {
	t.Helper()
	var usage map[string]int
	var events []string
	for _, frame := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var event, data string
		for _, line := range strings.Split(frame, "\n") {
			if value, ok := strings.CutPrefix(line, "event: "); ok {
				event = value
			}
			if value, ok := strings.CutPrefix(line, "data: "); ok {
				data = value
			}
		}
		events = append(events, event)
		var payload struct {
			Type  string         `json:"type"`
			Usage map[string]int `json:"usage"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		testutil.NoError(t, json.Unmarshal([]byte(data), &payload), "decode Messages SSE frame: %v")
		testutil.Equal(t, payload.Type, event)
		if event == "message_delta" {
			if usage != nil {
				t.Fatal("received more than one terminal message_delta")
			}
			testutil.Equal(t, payload.Delta.StopReason, "end_turn")
			usage = payload.Usage
		}
	}
	testutil.Falsef(t, !slices.Equal(events, []string{"message_start", "message_delta", "message_stop"}), "unexpected Messages SSE event order: %v", events)
	if usage == nil {
		t.Fatal("terminal message_delta has no usage object")
	}
	return usage
}

func TestTerminalMessagesUsagePreservesAuthoritativeUpstreamCounters(t *testing.T) {
	sh, rec := newTerminalUsageTestHandler(t)
	// Open with an estimate, then replace it with the authoritative counters.
	sh.mu.Lock()
	sh.writeMessageStartLocked("test-model", 999, 0)
	sh.useUpstreamUsage = true
	sh.inputTokens = 3
	sh.outputTokens = 5
	sh.cacheWriteTokens = 11686
	sh.cachedInputTokens = 0
	sh.mu.Unlock()

	sh.finishResponse("end_turn")

	usage := terminalMessagesUsage(t, rec.Body.String())
	want := map[string]int{
		"input_tokens":                3,
		"output_tokens":               5,
		"cache_creation_input_tokens": 11686,
		"cache_read_input_tokens":     0,
	}
	testutil.Falsef(t, !maps.Equal(usage, want), "terminal usage = %v, want %v", usage, want)
}

func TestTerminalMessagesUsageWithoutUpstreamRemainsOutputOnly(t *testing.T) {
	sh, rec := newTerminalUsageTestHandler(t)
	sh.mu.Lock()
	sh.useUpstreamUsage = false
	sh.inputTokens = 0
	sh.mu.Unlock()
	sh.addOutputTokens("hello world")

	sh.finishResponse("end_turn")

	usage := terminalMessagesUsage(t, rec.Body.String())
	testutil.Falsef(t, !maps.Equal(usage, map[string]int{"output_tokens": sh.outputTokens}), "terminal usage must remain output-only, got %v", usage)
	for _, key := range []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"} {
		if _, present := usage[key]; present {
			t.Errorf("terminal usage invented %s without upstream usage", key)
		}
	}
}
