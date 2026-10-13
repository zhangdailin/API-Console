package middleware

import (
	"sync"
	"time"
)

// RateLimiter implements a scalable token-bucket rate limiter keyed by IP.
type RateLimiter struct {
	entries     sync.Map
	maxAttempts int
	window      time.Duration
}

type limiterEntry struct {
	mu        sync.Mutex
	tokens    float64
	lastVisit time.Time
}

// NewRateLimiter creates a rate limiter that allows maxAttempts within the
// given window duration per IP address.
func NewRateLimiter(maxAttempts int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		maxAttempts: maxAttempts,
		window:      window,
	}
	go rl.cleanupLoop()
	return rl
}

// Allow reports whether the given IP is allowed to make another attempt.
func (rl *RateLimiter) Allow(ip string) bool {
	// Optimistic fast path: check if entry already exists
	if val, ok := rl.entries.Load(ip); ok {
		entry := val.(*limiterEntry)
		return rl.processExistingEntry(entry)
	}

	// Slow path: allocation and store
	entry := &limiterEntry{
		tokens:    float64(rl.maxAttempts - 1), // Pre-consume 1 token for this request
		lastVisit: time.Now(),
	}
	val, loaded := rl.entries.LoadOrStore(ip, entry)
	if !loaded {
		// New entry was stored — first request from this IP, already consumed 1 token
		return true
	}

	// Existing entry found (race condition during LoadOrStore)
	return rl.processExistingEntry(val.(*limiterEntry))
}

// CheckCredentials serializes password attempts per source. Only failures
// consume tokens, so authenticated polling cannot exhaust the login budget.
func (rl *RateLimiter) CheckCredentials(ip string, verify func() bool) (valid, allowed bool) {
	valid, allowed, _ = rl.CheckCredentialsWithError(ip, func() (bool, error) { return verify(), nil })
	return valid, allowed
}

// Backend failures are not incorrect passwords and must not spend the budget.
func (rl *RateLimiter) CheckCredentialsWithError(ip string, verify func() (bool, error)) (valid, allowed bool, err error) {
	if rl == nil {
		valid, err = verify()
		return valid, true, err
	}
	value, _ := rl.entries.LoadOrStore(ip, &limiterEntry{tokens: float64(rl.maxAttempts), lastVisit: time.Now()})
	entry := value.(*limiterEntry)
	entry.mu.Lock()
	defer entry.mu.Unlock()
	now := time.Now()
	entry.tokens = min(float64(rl.maxAttempts), entry.tokens+now.Sub(entry.lastVisit).Seconds()*float64(rl.maxAttempts)/rl.window.Seconds())
	entry.lastVisit = now
	if entry.tokens < 1 {
		return false, false, nil
	}
	valid, err = verify()
	if !valid && err == nil {
		entry.tokens--
	}
	return valid, true, err
}

func (rl *RateLimiter) processExistingEntry(entry *limiterEntry) bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(entry.lastVisit)

	// Replenish tokens based on elapsed time
	ratePerSec := float64(rl.maxAttempts) / rl.window.Seconds()
	entry.tokens += elapsed.Seconds() * ratePerSec
	if entry.tokens > float64(rl.maxAttempts) {
		entry.tokens = float64(rl.maxAttempts)
	}

	entry.lastVisit = now

	if entry.tokens >= 1 {
		entry.tokens--
		return true
	}

	return false
}

// cleanupLoop periodically removes expired entries to prevent unbounded
// memory growth.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute) // Less frequent cleanup needed
	defer ticker.Stop()
	for range ticker.C {
		rl.cleanup()
	}
}

func (rl *RateLimiter) cleanup() {
	now := time.Now()
	// TTL is twice the window size to ensure we don't prematurely delete active entries
	ttl := rl.window * 2

	rl.entries.Range(func(key, value interface{}) bool {
		entry := value.(*limiterEntry)
		entry.mu.Lock()
		lastVisit := entry.lastVisit
		entry.mu.Unlock()

		if now.Sub(lastVisit) > ttl {
			rl.entries.Delete(key)
		}
		return true
	})
}
