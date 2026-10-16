package grok

import (
	"strings"
	"sync"
	"time"
)

// teamCooldownRegistry tracks upstream resource-exhausted 429 cooldowns at
// (scope, teamID, model) granularity. When one account in an xAI team hits a
// rate limit for a model, sibling accounts sharing the team should not blindly
// retry the same model. This replaces
// the previous single global consoleTeamCooldownUntil with a bounded map.
const (
	teamCooldownMaxSize = 4096
	teamCooldownGCAfter = time.Minute
)

type teamCooldownEntry struct {
	until time.Time
}

// teamCooldownRegistry is a process-local, bounded, per-key cooldown store.
type teamCooldownRegistry struct {
	mu      sync.Mutex
	entries map[string]teamCooldownEntry
	lastGC  time.Time
}

var teamCooldown = newTeamCooldownRegistry()

func newTeamCooldownRegistry() *teamCooldownRegistry {
	return &teamCooldownRegistry{
		entries: make(map[string]teamCooldownEntry),
		lastGC:  time.Now(),
	}
}

func teamCooldownKey(scope RateLimitScope, teamID, model string) string {
	return string(scope) + "|" + strings.ToLower(strings.TrimSpace(teamID)) + "|" + strings.ToLower(strings.TrimSpace(model))
}

// Note records that a scope+team+model triple is cooling down until
// time.Now().Add(retryAfter). A zero teamID (not parsed) is a no-op so it does
// not block unrelated traffic.
func (r *teamCooldownRegistry) Note(scope RateLimitScope, teamID, model string, retryAfter time.Duration) {
	if strings.TrimSpace(teamID) == "" || strings.TrimSpace(model) == "" {
		return
	}
	now := time.Now()
	if retryAfter <= 0 {
		retryAfter = time.Minute
	}
	key := teamCooldownKey(scope, teamID, model)
	r.mu.Lock()
	until := now.Add(retryAfter)
	if cur, ok := r.entries[key]; ok && cur.until.After(until) {
		r.mu.Unlock()
		return
	}
	r.entries[key] = teamCooldownEntry{until: until}
	r.maybeGC(now)
	r.mu.Unlock()
	// Redis is deliberately outside the local registry lock. A slow or failed
	// shared backend must not serialize unrelated in-process model checks.
	if backend := grokLimitsBackend(); backend != nil {
		_ = backend.noteCooldown(scope, teamID, model, retryAfter)
	}
}

// RetryAfterFor returns the remaining cooldown for a scope+team+model, or 0 if
// none is active. An expired entry is removed.
func (r *teamCooldownRegistry) RetryAfterFor(scope RateLimitScope, teamID, model string) time.Duration {
	remaining := r.localRetryAfterFor(scope, teamID, model)
	if backend := grokLimitsBackend(); backend != nil {
		if shared := backend.cooldownRemaining(scope, teamID, model); shared > remaining {
			remaining = shared
		}
	}
	return remaining
}

func (r *teamCooldownRegistry) localRetryAfterFor(scope RateLimitScope, teamID, model string) time.Duration {
	if strings.TrimSpace(teamID) == "" {
		return 0
	}
	key := teamCooldownKey(scope, teamID, model)
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
func (r *teamCooldownRegistry) maybeGC(now time.Time) {
	if len(r.entries) < teamCooldownMaxSize && now.Sub(r.lastGC) < teamCooldownGCAfter {
		return
	}
	for key, entry := range r.entries {
		if !entry.until.After(now) {
			delete(r.entries, key)
		}
	}
	r.lastGC = now
}
