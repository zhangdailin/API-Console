package grok

import (
	"context"
	"encoding/base64"
	"testing"

	"encoding/json"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// testReplayCipher produces a distinct but equally valid opaque cipher per seed,
// so dedup tests never collide by accident.
func testReplayCipher(seed byte) string {
	data := make([]byte, 128)
	for i := range data {
		data[i] = byte(i) ^ seed
	}
	return base64.RawStdEncoding.EncodeToString(data)
}

func TestNormalizeReplayItemsRequiresAnAnchor(t *testing.T) {
	reasoning := map[string]any{"type": "reasoning", "encrypted_content": testReplayCipher(1)}
	message := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}}

	items, ok := normalizeReplayItems([]any{reasoning, message})
	testutil.Falsef(t, !ok || len(items) != 2, "items=%v ok=%v", items, ok)
	// Reasoning items must carry only the portable shape; extra keys 400 upstream.
	normalized, _ := items[0].(map[string]any)
	testutil.Equal(t, len(normalized), 3)
	// A message alone is not replayable.
	_, ok = normalizeReplayItems([]any{message})
	testutil.False(t, ok, "message-only replay should not be anchorable")
	// An undecodable cipher is dropped, so it cannot act as the anchor either.
	bad := map[string]any{"type": "reasoning", "encrypted_content": "gAAAA-not-a-cipher"}
	items, ok = normalizeReplayItems([]any{bad, message})
	testutil.Falsef(t, ok || len(items) != 1, "invalid cipher items=%v ok=%v", items, ok)
}

func TestFilterReplayItemsDropsWhatTheRequestAlreadyHas(t *testing.T) {
	cipher := testReplayCipher(2)
	items := []any{
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": cipher},
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "exec", "arguments": "{}"},
	}
	// The request already carries this cipher, so nothing is replayed.
	duplicate := []any{
		map[string]any{"type": "reasoning", "encrypted_content": cipher},
		map[string]any{"role": "user", "content": "next"},
	}
	got := filterReplayItemsForInput(duplicate, items)
	testutil.Falsef(t, len(got) != 0, "duplicate replay=%v", got)
	// A tool call whose output the request does not carry is not replayable.
	plain := []any{map[string]any{"role": "user", "content": "next"}}
	got = filterReplayItemsForInput(plain, items)
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, chatwire.ParseLooseStringAny(got[0].(map[string]any)["type"]), "reasoning")
}

func TestFilterReplayItemsMatchesPrefixedToolCallIDs(t *testing.T) {
	items := []any{
		map[string]any{"type": "function_call", "call_id": "call_1", "name": "exec", "arguments": "{}"},
	}
	// The client's own output uses the Anthropic-style prefix for the same call.
	input := []any{
		map[string]any{"type": "function_call_output", "call_id": "toolu_call_1", "output": "done"},
		map[string]any{"role": "user", "content": "next"},
	}
	got := filterReplayItemsForInput(input, items)
	testutil.Equal(t, len(got), 1)
	// The replayed call is rebound to the spelling the client already uses.
	testutil.Equal(t, chatwire.ParseLooseStringAny(got[0].(map[string]any)["call_id"]), "toolu_call_1")
}

func TestInsertReplayItemsPlacesBeforeMatchingToolOutput(t *testing.T) {
	items := []any{
		map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": testReplayCipher(3)},
		map[string]any{"type": "function_call", "call_id": "call_9", "name": "exec", "arguments": "{}"},
	}
	input := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": "go"},
		map[string]any{"type": "function_call_output", "call_id": "call_9", "output": "done"},
	}
	got := insertReplayItems(input, items)
	testutil.Equal(t, len(got), 5)
	// The replay must land ahead of its own tool output, never after it.
	for index, want := range []string{"", "", "reasoning", "function_call", "function_call_output"} {
		if want == "" {
			continue
		}
		testutil.Equal(t, chatwire.ParseLooseStringAny(got[index].(map[string]any)["type"]), want)
	}
}

func TestReplayItemsRejectLegacyCipher(t *testing.T) {
	var old store.StoredReasoningReplay
	testutil.NoError(t, json.Unmarshal([]byte(`{"encrypted_content":"old-cipher"}`), &old))
	testutil.Equal(t, len(replayItemsFromStored(&old)), 0)
	encoded := json.RawMessage(`{"type":"reasoning","encrypted_content":"new-cipher"}`)
	testutil.Equal(t, len(replayItemsFromStored(&store.StoredReasoningReplay{Items: []json.RawMessage{encoded}})), 1)
}

func TestCaptureReasoningReplayClearsStateWithoutAnchor(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	h.storeReasoningReplay("grok-4.6", "session", validTestReplayCipher())
	// The completed response carries only a message, so nothing is anchorable.
	h.captureReasoningReplayFromMap(context.Background(), "grok-4.6", "session", map[string]any{
		"output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "hi"}}},
		},
	})
	items := h.loadReasoningReplayItems("grok-4.6", "session")
	testutil.Falsef(t, len(items) != 0, "stale state kept: %v", items)
}

func TestCaptureReasoningReplayFromStream(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	cipher := testReplayCipher(5)
	stream := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"" + cipher + "\"}]}}\n\n"
	h.captureReasoningReplay(context.Background(), "grok-4.6", "session", []byte(stream))
	items := h.loadReasoningReplayItems("grok-4.6", "session")
	testutil.Equal(t, len(items), 1)
	testutil.Equal(t, chatwire.ParseLooseStringAny(items[0].(map[string]any)["encrypted_content"]), cipher)
}

func TestReasoningReplayCacheReturnsDeepCopies(t *testing.T) {
	h := &Handler{affinity: map[string]sessionAffinityEntry{}, replay: map[string]reasoningReplayEntry{}}
	cipher := testReplayCipher(7)
	original := []any{map[string]any{
		"type": "reasoning", "encrypted_content": cipher,
		"summary": []any{map[string]any{"type": "summary_text", "text": "stable"}},
	}}
	h.storeReasoningReplayItems("grok-4.6", "session", original)

	loaded := h.loadReasoningReplayItems("grok-4.6", "session")
	loadedItem := loaded[0].(map[string]any)
	loadedItem["encrypted_content"] = "mutated"
	loadedItem["summary"].([]any)[0].(map[string]any)["text"] = "mutated"

	again := h.loadReasoningReplayItems("grok-4.6", "session")
	againItem := again[0].(map[string]any)
	testutil.Equal(t, chatwire.ParseLooseStringAny(againItem["encrypted_content"]), cipher)
	summary := againItem["summary"].([]any)[0].(map[string]any)
	testutil.Equal(t, chatwire.ParseLooseStringAny(summary["text"]), "stable")
}
