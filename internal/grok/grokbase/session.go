package grokbase

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"strings"
	"sync"
	"time"
)

// SessionStateTTL is how long a session binding (and Build reasoning replay)
// is kept.
const SessionStateTTL = time.Hour

// Session is a request's stable, tenant- and model-isolated upstream identity.
type Session struct {
	Key    string
	Replay bool
	Model  string
}

type sessionContextKey struct{}

// SessionFromContext returns the session WithSession stored, or the zero value.
func SessionFromContext(ctx context.Context) Session {
	value, _ := ctx.Value(sessionContextKey{}).(Session)
	return value
}

// WithSession stores session on ctx; an empty key stores nothing.
func WithSession(ctx context.Context, session Session) context.Context {
	if strings.TrimSpace(session.Key) == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionContextKey{}, session)
}

// PrepareSession produces a stable, tenant- and model-isolated upstream
// identity. Explicit client identities permit encrypted reasoning replay;
// message-prefix fallback identities are affinity-only.
func PrepareSession(r *http.Request, model, explicit string, messages []chatwire.Message) Session {
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
			text := strings.TrimSpace(MessageContentText(message.Content))
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
			return Session{}
		}
		seed = system + "\x00" + firstUser
	}
	owner := "anonymous"
	if r != nil {
		if value := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context())); value != "" {
			owner = value
		}
	}
	model = strings.ToLower(strings.TrimSpace(model))
	source := fmt.Sprintf("orchids:grok-session:v1:%s:%s:%s", owner, model, seed)
	digest := sha256.Sum256([]byte(source))
	return Session{Key: hex.EncodeToString(digest[:]), Replay: explicitSession, Model: model}
}

func truncateSessionAnchor(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
	}
	return string(runes)
}

func affinityMapKey(session Session, provider string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + "\x00" + session.Model + "\x00" + session.Key
}

// Affinity binds a session to the account that served it, so a conversation
// keeps one upstream credential. Bindings live in process for SessionStateTTL
// and in the store, so another replica or a restart finds them. The zero value
// is ready to use.
type Affinity struct {
	mu      sync.Mutex
	locks   [64]sync.Mutex
	entries map[string]affinityEntry
}

type affinityEntry struct {
	AccountID int64
	ExpiresAt time.Time
}

func (h *Affinity) lock(key string) *sync.Mutex {
	digest := sha256.Sum256([]byte(key))
	return &h.locks[int(digest[0])%len(h.locks)]
}

// Account returns the account bound to ctx's session for provider, or 0.
func (h *Affinity) Account(ctx context.Context, st *store.Store, provider string) int64 {
	session := SessionFromContext(ctx)
	if h == nil || session.Key == "" {
		return 0
	}
	key := affinityMapKey(session, provider)
	h.mu.Lock()
	entry, ok := h.entries[key]
	if ok && time.Now().Before(entry.ExpiresAt) {
		h.mu.Unlock()
		return entry.AccountID
	}
	if ok {
		delete(h.entries, key)
	}
	h.mu.Unlock()
	if st == nil {
		return 0
	}
	// Serialize only this key with concurrent rebinding. The striped lock is held
	// across Redis I/O, but unrelated sessions continue independently.
	gate := h.lock(key)
	gate.Lock()
	defer gate.Unlock()
	h.mu.Lock()
	if latest, exists := h.entries[key]; exists && time.Now().Before(latest.ExpiresAt) {
		h.mu.Unlock()
		return latest.AccountID
	}
	h.mu.Unlock()
	persisted, err := st.GetSessionAffinity(ctx, provider, session.Model, session.Key)
	if err != nil || persisted == nil || persisted.AccountID == 0 {
		return 0
	}
	h.mu.Lock()
	if h.entries == nil {
		h.entries = make(map[string]affinityEntry)
	}
	h.entries[key] = affinityEntry{AccountID: persisted.AccountID, ExpiresAt: persisted.ExpiresAt}
	h.mu.Unlock()
	return persisted.AccountID
}

// Bind binds ctx's session for provider to accountID.
func (h *Affinity) Bind(ctx context.Context, st *store.Store, provider string, accountID int64) {
	session := SessionFromContext(ctx)
	if h == nil || session.Key == "" || accountID == 0 {
		return
	}
	key := affinityMapKey(session, provider)
	gate := h.lock(key)
	gate.Lock()
	defer gate.Unlock()
	expiresAt := time.Now().Add(SessionStateTTL)
	h.mu.Lock()
	if h.entries == nil {
		h.entries = make(map[string]affinityEntry)
	}
	h.entries[key] = affinityEntry{AccountID: accountID, ExpiresAt: expiresAt}
	h.mu.Unlock()
	if st != nil {
		_ = st.SaveSessionAffinity(ctx, &store.StoredSessionAffinity{
			Provider: provider, Model: session.Model, SessionKey: session.Key, AccountID: accountID,
		}, SessionStateTTL)
	}
}

// Unbind drops this session's binding to an account after a failure the
// account cannot recover from within the session. Without it the next turn
// returns to the same credential that just failed, which is how a cooling
// account kept being handed the same conversation.
func (h *Affinity) Unbind(ctx context.Context, st *store.Store, provider string, accountID int64) {
	session := SessionFromContext(ctx)
	if h == nil || session.Key == "" {
		return
	}
	key := affinityMapKey(session, provider)
	gate := h.lock(key)
	gate.Lock()
	defer gate.Unlock()
	h.mu.Lock()
	entry, existed := h.entries[key]
	if existed && (accountID == 0 || entry.AccountID == accountID) {
		delete(h.entries, key)
	}
	h.mu.Unlock()
	if !existed || st == nil {
		return
	}
	// Retire the persisted binding instead of waiting out its TTL. The store
	// rejects a zero account id, so the same binding is rewritten with a
	// one-second lifetime: it is gone before the next turn, and a concurrent
	// reader sees the account it was already using rather than a blank.
	_ = st.SaveSessionAffinity(ctx, &store.StoredSessionAffinity{
		Provider: provider, Model: session.Model, SessionKey: session.Key,
		AccountID: entry.AccountID,
	}, time.Second)
}

// MessageContentText joins the text parts of a chat message content value.
func MessageContentText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, part := range v {
			m, ok := part.(map[string]any)
			if !ok {
				continue
			}
			t := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
			switch t {
			case "text", "input_text":
				if s := strings.TrimSpace(fmt.Sprint(m["text"])); s != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(s)
				}
			}
		}
		return b.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
