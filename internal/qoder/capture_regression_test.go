package qoder

import (
	"encoding/json"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"strings"
	"testing"
)

// Reconstructed from the observed dump fields; no credentials or private logs.
func TestCaptureUsageAndTimings(t *testing.T) {
	body := envelope(`{"choices":[{"index":0,"delta":{"content":"测试123123"},"finish_reason":"stop"}],"usage":{"prompt_tokens":27260,"completion_tokens":48,"total_tokens":27308,"billable":false,"credits":0.4885985714285714,"prompt_tokens_details":{"cacheable_tokens":27254,"cached_tokens":0}}}`) + "event:finish\ndata:{\"firstTokenDuration\":1556,\"totalDuration\":2577,\"serverDuration\":89}\n\n"
	var text strings.Builder
	r, e := consumeStreamObserved(strings.NewReader(body), func(m upstream.SSEMessage) {
		if m.Type == "model.text-delta" {
			if v, ok := m.Event["delta"].(string); ok {
				text.WriteString(v)
			}
		}
	}, nil)
	testutil.NoError(t, e)
	testutil.Falsef(t, text.String() != "测试123123" || r.ToolCallCount != 0, "text=%q calls=%d", text.String(), r.ToolCallCount)
	testutil.Falsef(t, r.Usage["billable"] != false || r.Usage["firstTokenDuration"] != int64(1556) || r.Usage["cacheable_tokens"] != 27254, "metadata lost: %v", r.Usage)
	_, ok := r.Usage["cacheWriteTokens"]
	testutil.False(t, ok, "cacheable tokens counted as writes")
}
func TestReasoningItemSurvivesToolHistory(t *testing.T) {
	var msg prompt.Message
	e := json.Unmarshal([]byte(`{"role":"assistant","content":"","reasoning_item":{"opaque":"signature"},"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`), &msg)
	testutil.NoError(t, e)
	b, e := buildChatBodyProfile(upstream.UpstreamRequest{Messages: []prompt.Message{msg}}, modelEntry{Key: "qfmodel"}, "s", "r", "set", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, e)
	raw, e := decodeBody(b)
	testutil.NoError(t, e)
	var wire chatBody
	e = json.Unmarshal(raw, &wire)
	testutil.NoError(t, e)
	testutil.Falsef(t, len(wire.Messages) != 1 || string(wire.Messages[0].ReasoningItem) != `{"opaque":"signature"}` || len(wire.Messages[0].ToolCalls) != 1, "history lost: %s", raw)
}

func TestReasoningOnlyHistoryIsNotDropped(t *testing.T) {
	var msg prompt.Message
	testutil.NoError(t, json.Unmarshal([]byte(`{"role":"assistant","content":"","reasoning_item":{"opaque":"signature"}}`), &msg))
	messages, _ := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{msg}})
	testutil.Falsef(t, len(messages) != 1 || len(messages[0].ReasoningItem) == 0, "reasoning-only history lost: %#v", messages)
}
