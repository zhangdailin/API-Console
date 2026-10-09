package grok

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"sync"
	"time"

	"encoding/json"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

const grokSessionStateTTL = time.Hour

type sessionAffinityEntry struct {
	AccountID int64
	ExpiresAt time.Time
}

type reasoningReplayEntry struct {
	Items     []interface{}
	ExpiresAt time.Time
}

type grokSessionContext struct {
	Key    string
	Replay bool
	Model  string
}

type grokSessionContextKey struct{}

func sessionFromContext(ctx context.Context) grokSessionContext {
	value, _ := ctx.Value(grokSessionContextKey{}).(grokSessionContext)
	return value
}

func withGrokSession(ctx context.Context, session grokSessionContext) context.Context {
	if strings.TrimSpace(session.Key) == "" {
		return ctx
	}
	return context.WithValue(ctx, grokSessionContextKey{}, session)
}

// prepareGrokSession produces a stable, tenant- and model-isolated upstream
// identity. Explicit client identities permit encrypted reasoning replay;
// message-prefix fallback identities are affinity-only.
func prepareGrokSession(r *http.Request, model, explicit string, messages []chatwire.Message) grokSessionContext {
	seed, agentHint := strings.TrimSpace(explicit), ""
	if seed == "" && r != nil {
		// The explicit identities an agent client already sends: without them a
		// Claude Code or Codex session never gets a
		// stable key, so prompt caching and encrypted-reasoning replay silently
		// stay off for the clients that need them most.
		for _, header := range []string{
			"x-claude-code-session-id",
			"x-codex-session-id",
			"x-codex-conversation-id",
			"x-codex-window-id",
			"x-grok-session-id",
			"x-grok-conv-id",
			"x-conversation-id",
			"x-session-id",
			"session-id",
		} {
			if seed = strings.TrimSpace(r.Header.Get(header)); seed != "" {
				agentHint = header
				break
			}
		}
	}
	explicitSession := seed != ""
	if agentHint != "" {
		// Qualify the seed by its source: two clients that happen to use the same
		// identifier string must not share an upstream session.
		seed = agentHint + ":" + seed
	}
	if seed == "" {
		var system, firstUser string
		for _, message := range messages {
			role := strings.ToLower(strings.TrimSpace(message.Role))
			text := strings.TrimSpace(chatMessageContentText(message.Content))
			if text == "" {
				continue
			}
			if (role == "system" || role == "developer") && system == "" {
				system = truncateSessionAnchor(text, 100)
			}
			if role == "user" && firstUser == "" {
				firstUser = truncateSessionAnchor(text, 200)
			}
		}
		if firstUser == "" {
			return grokSessionContext{}
		}
		seed = system + "\x00" + firstUser
	}
	owner := "anonymous"
	if r != nil {
		if value := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context())); value != "" {
			owner = value
		}
	}
	source := fmt.Sprintf("orchids:grok-session:v1:%s:%s:%s", owner, normalizeModelID(model), seed)
	digest := sha256.Sum256([]byte(source))
	return grokSessionContext{Key: hex.EncodeToString(digest[:]), Replay: explicitSession, Model: normalizeModelID(model)}
}

func truncateSessionAnchor(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func affinityMapKey(session grokSessionContext, provider string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + session.Model + "\x00" + session.Key
}

func (h *Handler) affinityLock(key string) *sync.Mutex {
	digest := sha256.Sum256([]byte(key))
	return &h.affinityLocks[int(digest[0])%len(h.affinityLocks)]
}

func (h *Handler) affinityAccount(ctx context.Context, provider string) int64 {
	session := sessionFromContext(ctx)
	if h == nil || session.Key == "" {
		return 0
	}
	key := affinityMapKey(session, provider)
	h.sessionMu.Lock()
	entry, ok := h.affinity[key]
	if ok && time.Now().Before(entry.ExpiresAt) {
		h.sessionMu.Unlock()
		return entry.AccountID
	}
	if ok {
		delete(h.affinity, key)
	}
	h.sessionMu.Unlock()
	if h.lb == nil || h.lb.Store == nil {
		return 0
	}
	// Serialize only this key with concurrent rebinding. The striped lock is held
	// across Redis I/O, but unrelated sessions continue independently.
	gate := h.affinityLock(key)
	gate.Lock()
	defer gate.Unlock()
	h.sessionMu.Lock()
	if latest, exists := h.affinity[key]; exists && time.Now().Before(latest.ExpiresAt) {
		h.sessionMu.Unlock()
		return latest.AccountID
	}
	h.sessionMu.Unlock()
	persisted, err := h.lb.Store.GetSessionAffinity(ctx, provider, session.Model, session.Key)
	if err != nil || persisted == nil || persisted.AccountID == 0 {
		return 0
	}
	h.sessionMu.Lock()
	h.affinity[key] = sessionAffinityEntry{AccountID: persisted.AccountID, ExpiresAt: persisted.ExpiresAt}
	h.sessionMu.Unlock()
	return persisted.AccountID
}

func (h *Handler) bindAffinity(ctx context.Context, provider string, accountID int64) {
	session := sessionFromContext(ctx)
	if h == nil || session.Key == "" || accountID == 0 {
		return
	}
	key := affinityMapKey(session, provider)
	gate := h.affinityLock(key)
	gate.Lock()
	defer gate.Unlock()
	expiresAt := time.Now().Add(grokSessionStateTTL)
	h.sessionMu.Lock()
	h.affinity[key] = sessionAffinityEntry{AccountID: accountID, ExpiresAt: expiresAt}
	h.sessionMu.Unlock()
	if h.lb != nil && h.lb.Store != nil {
		_ = h.lb.Store.SaveSessionAffinity(ctx, &store.StoredSessionAffinity{
			Provider: provider, Model: session.Model, SessionKey: session.Key, AccountID: accountID,
		}, grokSessionStateTTL)
	}
}

// unbindAffinity drops this session's binding to an account after a failure the
// account cannot recover from within the session. Without it the next turn
// returns to the same credential that just failed, which is how a cooling
// account kept being handed the same conversation.
func (h *Handler) unbindAffinity(ctx context.Context, provider string, accountID int64) {
	session := sessionFromContext(ctx)
	if h == nil || session.Key == "" {
		return
	}
	key := affinityMapKey(session, provider)
	gate := h.affinityLock(key)
	gate.Lock()
	defer gate.Unlock()
	h.sessionMu.Lock()
	entry, existed := h.affinity[key]
	if existed && (accountID == 0 || entry.AccountID == accountID) {
		delete(h.affinity, key)
	}
	h.sessionMu.Unlock()
	if !existed || h.lb == nil || h.lb.Store == nil {
		return
	}
	// Retire the persisted binding instead of waiting out its TTL. The store
	// rejects a zero account id, so the same binding is rewritten with a
	// one-second lifetime: it is gone before the next turn, and a concurrent
	// reader sees the account it was already using rather than a blank.
	_ = h.lb.Store.SaveSessionAffinity(ctx, &store.StoredSessionAffinity{
		Provider: provider, Model: session.Model, SessionKey: session.Key,
		AccountID: entry.AccountID,
	}, time.Second)
}

func replayMapKey(model, key string) string {
	return normalizeModelID(model) + "\x00" + strings.TrimSpace(key)
}

// loadReasoningReplayItems returns the normalized replay items for a session.
func (h *Handler) loadReasoningReplayItems(model, key string) []interface{} {
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

// replayItemsFromStored reads the normalized item list, falling back to the
// legacy single cipher so state written by an older build still replays.
func replayItemsFromStored(persisted *store.StoredReasoningReplay) []interface{} {
	if len(persisted.Items) > 0 {
		items := make([]interface{}, 0, len(persisted.Items))
		for _, raw := range persisted.Items {
			var item map[string]interface{}
			if json.Unmarshal(raw, &item) == nil && item != nil {
				items = append(items, item)
			}
		}
		if len(items) > 0 {
			return items
		}
	}
	if !validReplayCipher(persisted.EncryptedContent) {
		return nil
	}
	return []interface{}{reasoningReplayItem(persisted.EncryptedContent)}
}

func reasoningReplayItem(encrypted string) map[string]interface{} {
	return map[string]interface{}{"type": "reasoning", "summary": []interface{}{}, "encrypted_content": encrypted}
}

// storeReasoningReplayItems persists a normalized item list for a session.
func (h *Handler) storeReasoningReplayItems(model, key string, items []interface{}) {
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
	h.replay[mapKey] = reasoningReplayEntry{Items: cacheItems, ExpiresAt: time.Now().Add(grokSessionStateTTL)}
	h.sessionMu.Unlock()
	// Persistence is outside sessionMu: slow Redis must not serialize unrelated
	// sessions. The in-memory generation prevents stale reads from resurrecting
	// data after an invalidate.
	if h.lb != nil && h.lb.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_ = h.lb.Store.SaveReasoningReplay(ctx, &store.StoredReasoningReplay{Model: model, SessionKey: key, Items: raw}, grokSessionStateTTL)
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
func cloneReplayItems(items []interface{}) []interface{} {
	if len(items) == 0 {
		return nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	var cloned []interface{}
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
	h.storeReasoningReplayItems(model, key, []interface{}{reasoningReplayItem(encrypted)})
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

func (h *Handler) applyNativeReasoningReplay(model, key string, payload map[string]interface{}) {
	if h == nil || payload == nil || strings.TrimSpace(chatwire.ParseLooseStringAny(payload["previous_response_id"])) != "" {
		return
	}
	items := h.loadReasoningReplayItems(model, key)
	if len(items) == 0 {
		return
	}
	input, ok := payload["input"].([]interface{})
	if !ok {
		if text, textOK := payload["input"].(string); textOK && strings.TrimSpace(text) != "" {
			input = []interface{}{map[string]interface{}{
				"type": "message", "role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": text}},
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

func ensureReasoningEncryptedInclude(payload map[string]interface{}) {
	includes := interfaceSlice(payload["include"])
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

func stripInjectedReasoningReplay(payload map[string]interface{}) bool {
	input, ok := payload["input"].([]interface{})
	if !ok {
		return false
	}
	changed := false
	next := make([]interface{}, 0, len(input))
	for _, raw := range input {
		item, _ := raw.(map[string]interface{})
		if item != nil && strings.EqualFold(interfaceString(item["type"]), "compaction") {
			next = append(next, raw)
			continue
		}
		if item != nil && strings.EqualFold(interfaceString(item["type"]), "reasoning") && interfaceString(item["encrypted_content"]) != "" {
			// A reasoning item without its proof is not portable. Preserve its
			// readable summary as an ordinary assistant message instead.
			parts := []interface{}{}
			for _, summary := range responses.InterfaceMaps(item["summary"]) {
				if value := interfaceString(summary["text"]); value != "" {
					parts = append(parts, map[string]interface{}{"type": "output_text", "text": value})
				}
			}
			changed = true
			if len(parts) > 0 {
				next = append(next, map[string]interface{}{"type": "message", "role": "assistant", "content": parts})
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
func preservesClientCompaction(payload map[string]interface{}, err error) bool {
	return payloadHasCompactionInput(payload) && isCompactionBlobDecodeError(err)
}

func payloadHasCompactionInput(payload map[string]interface{}) bool {
	input, ok := payload["input"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range input {
		if item, ok := raw.(map[string]interface{}); ok && strings.EqualFold(interfaceString(item["type"]), "compaction") {
			return true
		}
	}
	return false
}
