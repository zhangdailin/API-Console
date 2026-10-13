package middleware

import (
	"sync"
	"time"
)

// RateLimiter bounds failed password attempts per source with a token bucket.
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

// NewRateLimiter creates a failed-password budget of maxAttempts within
// the given window duration per source address.
func NewRateLimiter(maxAttempts int, window time.Duration) *RateLimiter {
	rl := &RateLimiter{
		maxAttempts: maxAttempts,
		window:      window,
	}
	go rl.cleanupLoop()
	return rl
}

// CheckCredentialsWithError serializes password attempts per source. Only
// incorrect passwords consume tokens; backend failures do not spend the budget.
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
