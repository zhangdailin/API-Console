package grok

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/responses"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/store"
)

type reasoningReplayEntry struct {
	Items     []any
	ExpiresAt time.Time
}

// The session's account binding lives in grokbase.Affinity; these pass it the
// plane's store.
func (h *Handler) affinityAccount(ctx context.Context, provider string) int64 {
	if h == nil {
		return 0
	}
	return h.affinity.Account(ctx, h.affinityStore(), provider)
}

func (h *Handler) bindAffinity(ctx context.Context, provider string, accountID int64) {
	if h == nil {
		return
	}
	h.affinity.Bind(ctx, h.affinityStore(), provider, accountID)
}

func (h *Handler) unbindAffinity(ctx context.Context, provider string, accountID int64) {
	if h == nil {
		return
	}
	h.affinity.Unbind(ctx, h.affinityStore(), provider, accountID)
}

func (h *Handler) affinityStore() *store.Store {
	if h.lb == nil {
		return nil
	}
	return h.lb.Store
}

func replayMapKey(model, key string) string {
	return normalizeModelID(model) + "\x00" + strings.TrimSpace(key)
}

// loadReasoningReplayItems returns the normalized replay items for a session.
func (h *Handler) loadReasoningReplayItems(model, key string) []any {
	if h == nil || strings.TrimSpace(key) == "" {
		return nil
	}
	h.sessionMu.Lock()
	mapKey := replayMapKey(model, key)
	entry, ok := h.replay[mapKey]
	if ok && time.Now().Before(entry.ExpiresAt) {
		cached := entry.Items
		h.sessionMu.Unlock()
		return cloneReplayItems(cached)
	}
	if ok {
		delete(h.replay, mapKey)
	}
	generation := h.replayGen[mapKey]
	h.sessionMu.Unlock()
	if h.lb == nil || h.lb.Store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	persisted, err := h.lb.Store.GetReasoningReplay(ctx, model, key)
	cancel()
	if err != nil || persisted == nil {
		return nil
	}
	items := replayItemsFromStored(persisted)
	if len(items) == 0 {
		return nil
	}
	cacheItems := cloneReplayItems(items)
	if len(cacheItems) == 0 {
		return nil
	}
	h.sessionMu.Lock()
	if latest, ok := h.replay[mapKey]; ok && time.Now().Before(latest.ExpiresAt) {
		h.sessionMu.Unlock()
		return cloneReplayItems(latest.Items)
	}
	if h.replayGen[mapKey] != generation {
		h.sessionMu.Unlock()
		return nil
	}
	if h.replay == nil {
		h.replay = map[string]reasoningReplayEntry{}
	}
	h.replay[mapKey] = reasoningReplayEntry{Items: cacheItems, ExpiresAt: persisted.ExpiresAt}
	h.sessionMu.Unlock()
	return cloneReplayItems(cacheItems)
}

// replayItemsFromStored reads current normalized item lists only.
func replayItemsFromStored(persisted *store.StoredReasoningReplay) []any {
	if persisted == nil {
		return nil
	}
	items := make([]any, 0, len(persisted.Items))
	for _, raw := range persisted.Items {
		var item map[string]any
		if json.Unmarshal(raw, &item) == nil && item != nil {
			items = append(items, item)
		}
	}
	return items
}

func reasoningReplayItem(encrypted string) map[string]any {
	return map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted}
}

// storeReasoningReplayItems persists a normalized item list for a session.
func (h *Handler) storeReasoningReplayItems(model, key string, items []any) {
	if h == nil || strings.TrimSpace(key) == "" || len(items) == 0 {
		return
	}
	cacheItems := cloneReplayItems(items)
	if len(cacheItems) == 0 {
		return
	}
	raw := make([]json.RawMessage, 0, len(cacheItems))
	for _, item := range cacheItems {
		encoded, err := json.Marshal(item)
		if err != nil {
			return
		}
		raw = append(raw, encoded)
	}
	mapKey := replayMapKey(model, key)
	h.sessionMu.Lock()
	if h.replay == nil {
		h.replay = map[string]reasoningReplayEntry{}
	}
	if h.replayGen == nil {
		h.replayGen = make(map[string]uint64)
	}
	h.replayGen[mapKey]++
	generation := h.replayGen[mapKey]
	h.replay[mapKey] = reasoningReplayEntry{Items: cacheItems, ExpiresAt: time.Now().Add(grokbase.SessionStateTTL)}
	h.sessionMu.Unlock()
	// Persistence is outside sessionMu: slow Redis must not serialize unrelated
	// sessions. The in-memory generation prevents stale reads from resurrecting
	// data after an invalidate.
	if h.lb != nil && h.lb.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_ = h.lb.Store.SaveReasoningReplay(ctx, &store.StoredReasoningReplay{Model: model, SessionKey: key, Items: raw}, grokbase.SessionStateTTL)
		cancel()
		h.sessionMu.Lock()
		stale := h.replayGen[mapKey] != generation
		h.sessionMu.Unlock()
		if stale {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			_ = h.lb.Store.DeleteReasoningReplay(cleanupCtx, model, key)
			cleanupCancel()
		}
	}
}

// cloneReplayItems prevents request normalizers from mutating cached maps that
// another request may be reading. Replay values are already JSON-compatible,
// making a JSON round trip a compact and reliable deep copy.
func cloneReplayItems(items []any) []any {
	if len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	var cloned []any
	if json.Unmarshal(raw, &cloned) != nil {
		return nil
	}
	return cloned
}

// storeReasoningReplay is the single-cipher convenience used by paths that only
// observe one opaque reasoning item.
func (h *Handler) storeReasoningReplay(model, key, encrypted string) {
	if !validReplayCipher(encrypted) {
		return
	}
	h.storeReasoningReplayItems(model, key, []any{reasoningReplayItem(encrypted)})
}

func validReplayCipher(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 8<<20 || strings.HasPrefix(value, "gAAAA") || strings.Contains(value, "=") {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(decoded) < 50 {
		return false
	}
	var counts [256]int
	for _, b := range decoded {
		counts[b]++
	}
	entropy := 0.0
	for _, n := range counts {
		if n > 0 {
			p := float64(n) / float64(len(decoded))
			entropy -= p * math.Log2(p)
		}
	}
	return entropy/math.Log2(float64(min(len(decoded), 256))) >= 0.85
}

func (h *Handler) applyNativeReasoningReplay(model, key string, payload map[string]any) {
	if h == nil || payload == nil || strings.TrimSpace(chatwire.ParseLooseStringAny(payload["previous_response_id"])) != "" {
		return
	}
	items := h.loadReasoningReplayItems(model, key)
	if len(items) == 0 {
		return
	}
	input, ok := payload["input"].([]any)
	if !ok {
		if text, textOK := payload["input"].(string); textOK && strings.TrimSpace(text) != "" {
			input = []any{map[string]any{
				"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}},
			}}
		} else {
			return
		}
	}
	if len(input) == 0 {
		return
	}
	// Filtering already drops anything the request itself carries, so a client
	// that resends its own history is never duplicated.
	filtered := filterReplayItemsForInput(input, items)
	if len(filtered) == 0 {
		return
	}
	payload["input"] = backfillReasoningForCalls(insertReplayItems(input, filtered), items)
	ensureReasoningEncryptedInclude(payload)
}

func ensureReasoningEncryptedInclude(payload map[string]any) {
	includes := responses.InterfaceSlice(payload["include"])
	for _, value := range includes {
		if strings.EqualFold(strings.TrimSpace(fmt.Sprint(value)), "reasoning.encrypted_content") {
			return
		}
	}
	payload["include"] = append(includes, "reasoning.encrypted_content")
}

func (h *Handler) clearReasoningReplay(ctx context.Context, model, key string) {
	if h == nil || strings.TrimSpace(key) == "" {
		return
	}
	mapKey := replayMapKey(model, key)
	h.sessionMu.Lock()
	if h.replayGen == nil {
		h.replayGen = make(map[string]uint64)
	}
	h.replayGen[mapKey]++
	delete(h.replay, mapKey)
	h.sessionMu.Unlock()
	if h.lb != nil && h.lb.Store != nil {
		_ = h.lb.Store.DeleteReasoningReplay(ctx, model, key)
	}
}

func stripInjectedReasoningReplay(payload map[string]any) bool {
	input, ok := payload["input"].([]any)
	if !ok {
		return false
	}
	changed := false
	next := make([]any, 0, len(input))
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item != nil && strings.EqualFold(chatwire.ParseLooseStringAny(item["type"]), "compaction") {
			next = append(next, raw)
			continue
		}
		if item != nil && strings.EqualFold(chatwire.ParseLooseStringAny(item["type"]), "reasoning") && chatwire.ParseLooseStringAny(item["encrypted_content"]) != "" {
			// Keep the historical plan explicitly labelled; unlabelled summaries
			// teach the model that private analysis is an ordinary public answer.
			changed = true
			if summary := portableReasoningSummary(item); summary != nil {
				next = append(next, summary)
			}
			continue
		}
		next = append(next, raw)
	}
	if changed {
		payload["input"] = next
	}
	return changed
}

func isReasoningReplayDecodeError(err error) bool {
	if err == nil || upstreamStatus(err) != http.StatusBadRequest {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "invalid_encrypted_content") ||
		strings.Contains(text, "could not decrypt the provided encrypted_content") ||
		strings.Contains(text, "could not decode the compaction blob")
}

// isCompactionBlobDecodeError reports whether the rejection used Build's
// compaction-decode wording.
func isCompactionBlobDecodeError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "could not decode the compaction blob")
}

// preservesClientCompaction reports whether a recognized decode failure must be
// left untouched. Build historically reused the compaction-decode wording for
// opaque reasoning and session failures, so that wording only means a real
// compaction rejection when the request actually carried a compaction item.
// A plain encrypted-content rejection stays recoverable even then, because the
// recovery only rewrites reasoning items and never compaction state.
func preservesClientCompaction(payload map[string]any, err error) bool {
	return payloadHasCompactionInput(payload) && isCompactionBlobDecodeError(err)
}

func payloadHasCompactionInput(payload map[string]any) bool {
	input, ok := payload["input"].([]any)
	if !ok {
		return false
	}
	for _, raw := range input {
		if item, ok := raw.(map[string]any); ok && strings.EqualFold(chatwire.ParseLooseStringAny(item["type"]), "compaction") {
			return true
		}
	}
	return false
}
