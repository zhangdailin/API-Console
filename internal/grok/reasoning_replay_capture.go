package grok

import (
	"bytes"
	"context"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"

	"encoding/json"
)

// captureReasoningReplay stores the previous turn's portable output items so the
// next request can continue the same reasoning chain. A payload whose items all
// fail normalization clears the session state instead of leaving a stale entry
// that would be rejected again on the next request.
func (h *Handler) captureReasoningReplay(ctx context.Context, model, key string, raw []byte) {
	if h == nil || strings.TrimSpace(key) == "" || len(raw) == 0 {
		return
	}
	items, ok := replayItemsFromPayload(raw)
	if !ok {
		return
	}
	h.captureReasoningReplayItems(ctx, model, key, items)
}

func (h *Handler) captureReasoningReplayFromMap(ctx context.Context, model, key string, raw map[string]interface{}) {
	if h == nil || strings.TrimSpace(key) == "" || raw == nil {
		return
	}
	items, ok := extractReplayItems(raw)
	if !ok {
		return
	}
	h.captureReasoningReplayItems(ctx, model, key, items)
}

func (h *Handler) captureReasoningReplayItems(ctx context.Context, model, key string, items []interface{}) {
	normalized, ok := normalizeReplayItems(items)
	if !ok {
		h.clearReasoningReplay(ctx, model, key)
		return
	}
	h.storeReasoningReplayItems(model, key, normalized)
}

// replayItemsFromPayload accepts either a buffered JSON response or an SSE
// stream, and returns the portable output items of the completed response.
func replayItemsFromPayload(raw []byte) ([]interface{}, bool) {
	if items, ok := extractReplayItemsFromJSON(raw); ok {
		return items, true
	}
	var completed map[string]interface{}
	_ = responses.ReadSSE(bytes.NewReader(raw), func(_ string, data string) error {
		var event map[string]interface{}
		if json.Unmarshal([]byte(data), &event) != nil {
			return nil
		}
		switch chatwire.ParseLooseStringAny(event["type"]) {
		case "response.completed", "response.done":
			completed = event
		}
		return nil
	})
	if completed == nil {
		return nil, false
	}
	return extractReplayItems(completed)
}

func extractReplayItemsFromJSON(raw []byte) ([]interface{}, bool) {
	var root map[string]interface{}
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, false
	}
	return extractReplayItems(root)
}
