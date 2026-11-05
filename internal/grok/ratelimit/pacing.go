package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// tokenBucket is a simple thread-safe token bucket rate limiter.
type tokenBucket struct {
	rate       float64   // tokens per second
	burst      float64   // max tokens (burst capacity)
	tokens     float64   // current tokens
	lastUpdate time.Time // last token refill
	mu         sync.Mutex
}

func newTokenBucket(ratePerSec, burst float64) *tokenBucket {
	return &tokenBucket{
		rate:       ratePerSec,
		burst:      burst,
		tokens:     burst, // start full
		lastUpdate: time.Now(),
	}
}

// wait blocks until a token is available or ctx is cancelled.
// Returns nil if a token was acquired, or ctx.Err() if cancelled.
func (tb *tokenBucket) wait(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tb.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(tb.lastUpdate).Seconds()
		tb.tokens += elapsed * tb.rate
		if tb.tokens > tb.burst {
			tb.tokens = tb.burst
		}
		tb.lastUpdate = now

		if tb.tokens >= 1.0 {
			tb.tokens -= 1.0
			tb.mu.Unlock()
			return nil
		}
		// Calculate wait time for next token
		waitTime := time.Duration((1.0 - tb.tokens) / tb.rate * float64(time.Second))
		tb.mu.Unlock()

		if waitTime < 50*time.Millisecond {
			waitTime = 50 * time.Millisecond
		}
		if waitTime > 5*time.Second {
			waitTime = 5 * time.Second
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitTime):
		}
	}
}

// Optional pacing buckets contain credential fingerprints, never credentials.
var (
	endpointRateLimiters  = map[string]*tokenBucket{}
	endpointRateLimiterMu sync.Mutex
)

// WaitPacing takes one pacing token for key at rate requests per second. It
// uses the shared Redis pacer when one is configured and falls back to a
// process-local bucket when Redis is unavailable. rate <= 0 disables pacing.
func WaitPacing(ctx context.Context, key string, rate float64) error {
	if rate <= 0 {
		return nil
	}
	if backend := sharedBackend(); backend != nil {
		if err := backend.waitPacing(ctx, key, rate); err == nil {
			slog.Debug("Rate limiter: distributed token acquired", "endpoint", key)
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			slog.Warn("Rate limiter: Redis pacing unavailable; using process-local fallback", "error", err)
		}
	}
	endpointRateLimiterMu.Lock()
	tb := endpointRateLimiters[key]
	if tb == nil || tb.rate != rate {
		if len(endpointRateLimiters) >= MaxEntries {
			// Evict only an idle bucket; never split an active account's limiter.
			for existing, candidate := range endpointRateLimiters {
				candidate.mu.Lock()
				idle := time.Since(candidate.lastUpdate) > time.Minute
				candidate.mu.Unlock()
				if idle {
					delete(endpointRateLimiters, existing)
					break
				}
			}
			if len(endpointRateLimiters) >= MaxEntries {
				endpointRateLimiterMu.Unlock()
				return fmt.Errorf("grok pacing registry capacity reached")
			}
		}
		tb = newTokenBucket(rate, 1)
		endpointRateLimiters[key] = tb
	}
	endpointRateLimiterMu.Unlock()
	if err := tb.wait(ctx); err != nil {
		return err
	}
	slog.Debug("Rate limiter: token acquired", "endpoint", key)
	return nil
}
