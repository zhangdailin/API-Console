package ratelimit

import (
	"strings"
	"sync"
	"time"
)

// Registry tracks upstream resource-exhausted 429 cooldowns at
// (scope, identity, model) granularity. When one account in an xAI team hits a
// rate limit for a model, sibling accounts sharing the team should not blindly
// retry the same model.
//
// Identities carry their plane prefix ("build:", "web:"), so both planes share
// one registry without seeing each other's entries.
const (
	// MaxEntries bounds the registry and the pacing buckets. It covers both
	// planes, which kept 4096 each before they shared the registry.
	MaxEntries      = 8192
	cooldownGCAfter = time.Minute
)

type cooldownEntry struct {
	until time.Time
}

// The registry itself is process-local and bounded; Note and RetryAfterFor
// also consult the Redis backend when one is configured.
type Registry struct {
	mu      sync.Mutex
	entries map[string]cooldownEntry
	lastGC  time.Time
}

// Cooldowns is the process registry shared by both planes.
var Cooldowns = NewRegistry()

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		entries: make(map[string]cooldownEntry),
		lastGC:  time.Now(),
	}
}

func cooldownKey(scope Scope, teamID, model string) string {
	return string(scope) + "|" + strings.ToLower(strings.TrimSpace(teamID)) + "|" + strings.ToLower(strings.TrimSpace(model))
}

// Note records that a scope+team+model triple is cooling down until
// time.Now().Add(retryAfter). A zero teamID (not parsed) is a no-op so it does
// not block unrelated traffic.
func (r *Registry) Note(scope Scope, teamID, model string, retryAfter time.Duration) {
	if strings.TrimSpace(teamID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	now := time.Now()
	if retryAfter <= 0 {
		retryAfter = time.Minute
	}
	key := cooldownKey(scope, teamID, model)
	r.mu.Lock()
	until := now.Add(retryAfter)
	if cur, ok := r.entries[key]; ok && cur.until.After(until) {
		r.mu.Unlock()
		return
	}
	r.entries[key] = cooldownEntry{until: until}
	r.maybeGC(now)
	r.mu.Unlock()
	// Redis is deliberately outside the local registry lock. A slow or failed
	// shared backend must not serialize unrelated in-process model checks.
	if backend := sharedBackend(); backend != nil {
		_ = backend.noteCooldown(scope, teamID, model, retryAfter)
	}
}

// RetryAfterFor returns the remaining cooldown for a scope+team+model, or 0 if
// none is active. An expired entry is removed.
func (r *Registry) RetryAfterFor(scope Scope, teamID, model string) time.Duration {
	remaining := r.LocalRetryAfterFor(scope, teamID, model)
	if backend := sharedBackend(); backend != nil {
		if shared := backend.cooldownRemaining(scope, teamID, model); shared > remaining {
			remaining = shared
		}
	}
	return remaining
}

// LocalRetryAfterFor is RetryAfterFor without the Redis read.
func (r *Registry) LocalRetryAfterFor(scope Scope, teamID, model string) time.Duration {
	if strings.TrimSpace(teamID) == "" {
		return 0
	}
	key := cooldownKey(scope, teamID, model)
	now := time.Now()
	r.mu.Lock()
	cur, ok := r.entries[key]
	if ok && !cur.until.After(now) {
		delete(r.entries, key)
		ok = false
	}
	remaining := time.Duration(0)
	if ok {
		remaining = cur.until.Sub(now)
	}
	r.mu.Unlock()
	return remaining
}

// maybeGC prunes expired entries when the store exceeds a bound or a GC
// interval elapses. Caller must hold r.mu.
func (r *Registry) maybeGC(now time.Time) {
	if len(r.entries) < MaxEntries && now.Sub(r.lastGC) < cooldownGCAfter {
		return
	}
	for key, entry := range r.entries {
		if !entry.until.After(now) {
			delete(r.entries, key)
		}
	}
	r.lastGC = now
}
