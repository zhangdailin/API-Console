package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

const grokSessionStateTTL = time.Hour

type sessionAffinityEntry struct {
	AccountID int64
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
func prepareGrokSession(r *http.Request, model, explicit string, messages []ChatMessage) grokSessionContext {
	seed, agentHint := strings.TrimSpace(explicit), ""
	if seed == "" && r != nil {
		// The explicit identities an agent client already sends. grok2api reads
		// the same ones: without them a Claude Code or Codex session never gets a
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
