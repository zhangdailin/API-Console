package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	sessionTokenLength = 32
	sessionTTL         = 7 * 24 * time.Hour
)

// SessionBackend persists admin sessions outside this process. The admin UI
// polls the gateway continuously, so a session that exists only in memory makes
// every restart — including every deploy — look like the site is broken: the
// browser keeps presenting a cookie the fresh process has never seen, the
// polling endpoints answer 401 and the UI bounces back to the login page.
type SessionBackend interface {
	SaveSession(ctx context.Context, token string, expiry time.Time) error
	HasSession(ctx context.Context, token string) (bool, error)
	DeleteSession(ctx context.Context, token string)
}

var (
	sessionBackendMu sync.RWMutex
	sessionBackend   SessionBackend
)

// SetSessionBackend installs the durable session store. Passing nil keeps the
// in-process map. With a backend configured, authentication fails closed when
// revocation cannot be checked; a cached session must never undo revocation.
func SetSessionBackend(backend SessionBackend) {
	sessionBackendMu.Lock()
	sessionBackend = backend
	sessionBackendMu.Unlock()
}

func durableSessionBackend() SessionBackend {
	sessionBackendMu.RLock()
	defer sessionBackendMu.RUnlock()
	return sessionBackend
}

// CurrentAdminCredentials reads shared credentials on every password entrance.
// A replica's startup snapshot must never restore a revoked password.
func CurrentAdminCredentials(ctx context.Context, user, password string) (string, string, error) {
	if backend, ok := durableSessionBackend().(interface {
		AdminCredentials(context.Context, string, string) (string, string, error)
	}); ok {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		return backend.AdminCredentials(ctx, user, password)
	}
	return user, password, nil
}

type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]time.Time
}

// globalSessionStore mirrors every session this process issued. It is the
// authoritative store when no durable backend is configured.
var globalSessionStore = &SessionStore{
	sessions: make(map[string]time.Time),
}

func init() {
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			cleanupExpiredSessions()
		}
	}()
}

func GenerateSessionToken() (string, error) {
	return GenerateSessionTokenForPassword("")
}

// GenerateSessionTokenForPassword rejects logins using stale replica credentials.
func GenerateSessionTokenForPassword(password string) (string, error) {
	bytes := make([]byte, sessionTokenLength)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("failed to generate session token: %w", err)
	}
	token := hex.EncodeToString(bytes)
	expiry := time.Now().Add(sessionTTL)

	if backend := durableSessionBackend(); backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		var err error
		if guarded, ok := backend.(interface {
			SaveSessionForPassword(context.Context, string, time.Time, string) error
		}); ok && password != "" {
			err = guarded.SaveSessionForPassword(ctx, token, expiry, password)
		} else {
			err = backend.SaveSession(ctx, token, expiry)
		}
		if err != nil {
			return "", fmt.Errorf("persist admin session: %w", err)
		}
	}

	rememberSession(token, expiry)

	return token, nil
}

func ValidateSessionToken(token string) bool {
	if strings.TrimSpace(token) == "" {
		return false
	}
	if backend := durableSessionBackend(); backend != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		valid, err := backend.HasSession(ctx, token)
		if err != nil {
			slog.Warn("Admin session revocation check unavailable", "error", err)
			return false
		}
		if !valid {
			forgetSession(token)
		}
		return valid
	}
	return memoryHasSession(token)
}

func InvalidateSessionToken(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	forgetSession(token)
	if backend := durableSessionBackend(); backend != nil {
		backend.DeleteSession(context.Background(), token)
	}
}

func rememberSession(token string, expiry time.Time) {
	globalSessionStore.mu.Lock()
	globalSessionStore.sessions[token] = expiry
	globalSessionStore.mu.Unlock()
}

// hasSession reads one session table under the store lock and forgets an entry
// whose TTL has already passed.
func (s *SessionStore) hasSession(table map[string]time.Time, token string) bool {
	s.mu.RLock()
	expiry, exists := table[token]
	s.mu.RUnlock()

	if !exists {
		return false
	}
	if time.Now().After(expiry) {
		forgetSession(token)
		return false
	}
	return true
}

func memoryHasSession(token string) bool {
	return globalSessionStore.hasSession(globalSessionStore.sessions, token)
}

func forgetSession(token string) {
	globalSessionStore.mu.Lock()
	delete(globalSessionStore.sessions, token)
	globalSessionStore.mu.Unlock()
}

func cleanupExpiredSessions() {
	globalSessionStore.mu.Lock()
	defer globalSessionStore.mu.Unlock()

	now := time.Now()
	for _, sessions := range []map[string]time.Time{
		globalSessionStore.sessions,
	} {
		for token, expiry := range sessions {
			if now.After(expiry) {
				delete(sessions, token)
			}
		}
	}
}

// ClearLocalSessions revokes sessions in deployments without a shared backend.
// Redis config writes rotate the durable generation in the same transaction.
func ClearLocalSessions() {
	globalSessionStore.mu.Lock()
	clear(globalSessionStore.sessions)
	globalSessionStore.mu.Unlock()
}
