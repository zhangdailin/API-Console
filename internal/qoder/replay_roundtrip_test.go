package qoder

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// TestReplayBodyChangesOnlyTheAttemptFields pins that a replay (refresh,
// failover or transient retry) re-encodes the body without losing anything:
// it is decoded into chatBody and marshaled again, so a wire field the struct
// does not model would silently vanish from every replayed request.
func TestReplayBodyChangesOnlyTheAttemptFields(t *testing.T) {
	enabled := true
	model := modelEntry{Key: "qmodel_38max", Name: "Qwen3.8-Max", Format: "openai", Source: "system", Enable: &enabled, IsReasoning: true, MaxInputTokens: 1000000}
	req := upstream.UpstreamRequest{
		Model:  "Qwen3.8-Max",
		System: []prompt.SystemItem{{Type: "text", Text: "be brief"}},
		Messages: []prompt.Message{
			{Role: "user", Content: prompt.MessageContent{Text: "hello"}},
			{Role: "assistant", Content: prompt.MessageContent{Text: "hi"}},
			{Role: "user", Content: prompt.MessageContent{Text: "again"}},
		},
		Tools: []any{map[string]any{"name": "read", "description": "read a file", "input_schema": map[string]any{"type": "object"}}},
	}
	original, err := buildChatBodyProfile(req, model, "session", "request-1", "set-1", "1.0.0", "", sceneBusinessProduct)
	testutil.NoError(t, err)
	replayed, err := refreshedReplayBody(original, "request-2")
	testutil.NoError(t, err)

	decode := func(encoded []byte) map[string]any {
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var out map[string]any
		testutil.NoError(t, json.Unmarshal(raw, &out))
		return out
	}
	before, after := decode(original), decode(replayed)
	testutil.Equal(t, after["request_id"], "request-2")
	testutil.Equal(t, after["chat_record_id"], "request-2")
	for _, body := range []map[string]any{before, after} {
		delete(body, "request_id")
		delete(body, "chat_record_id")
		delete(body, "is_retry")
		if business, ok := body["business"].(map[string]any); ok {
			delete(business, "begin_at")
		}
	}
	wantJSON, _ := json.Marshal(before)
	gotJSON, _ := json.Marshal(after)
	testutil.Equal(t, string(gotJSON), string(wantJSON))
}
