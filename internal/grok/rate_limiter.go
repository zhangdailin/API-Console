package grok

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

type syntheticCooldownError struct {
	*grokUpstreamError
	delay time.Duration
}

func newSyntheticCooldownError(identity, model string, delay time.Duration) error {
	delay = accountpolicy.BoundRateLimitCooldown(delay)
	return &syntheticCooldownError{
		grokUpstreamError: &grokUpstreamError{
			status: http.StatusTooManyRequests,
			header: http.Header{"Retry-After": {fmt.Sprint(int64((delay + time.Second - 1) / time.Second))}},
			body:   fmt.Sprintf("too_many_requests team %s model %s cooling down; retry-after=%s", identity, model, delay.Round(time.Second)),
		},
		delay: delay,
	}
}

func (e *syntheticCooldownError) Unwrap() error { return e.grokUpstreamError }

func (e *syntheticCooldownError) RetryAfter() time.Duration { return e.delay }

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

type rateLimitAccountContextKey struct{}

func withRateLimitAccount(ctx context.Context, acc *store.Account) context.Context {
	if acc == nil {
		return ctx
	}
	identity := ""
	if acc.TeamID != "" {
		identity = "team:" + acc.TeamID
	}
	if identity == "" && acc.ID != 0 {
		identity = fmt.Sprintf("account:%d", acc.ID)
	}
	return context.WithValue(ctx, rateLimitAccountContextKey{}, identity)
}

func rateLimitIdentity(ctx context.Context, token string) string {
	if identity, _ := ctx.Value(rateLimitAccountContextKey{}).(string); identity != "" {
		return identity
	}
	return "token:" + credentialAffinity(token)
}

// waitScopedRateLimit rejects an active team/account+model cooldown immediately,
// then consumes one pacing token. Request paths must rotate accounts instead of
// parking a request goroutine until an upstream 429 window expires.
func waitScopedRateLimit(ctx context.Context, provider, token, model string, rate float64) error {
	identity := provider + ":" + rateLimitIdentity(ctx, token)
	for _, scope := range []RateLimitScope{RateLimitScopeRPS, RateLimitScopeRPM} {
		for _, target := range util.UniqueStrings([]string{model, "*"}) {
			if remaining := teamCooldown.RetryAfterFor(scope, identity, target); remaining > 0 {
				return newSyntheticCooldownError(identity, target, remaining)
			}
		}
	}
	if rate <= 0 {
		return nil
	}
	endpointKey := identity
	if backend := grokLimitsBackend(); backend != nil {
		if err := backend.waitPacing(ctx, endpointKey, rate); err == nil {
			slog.Debug("Rate limiter: distributed token acquired", "endpoint", endpointKey)
			return nil
		} else if ctx.Err() != nil {
			return ctx.Err()
		} else {
			slog.Warn("Rate limiter: Redis pacing unavailable; using process-local fallback", "error", err)
		}
	}
	endpointRateLimiterMu.Lock()
	tb := endpointRateLimiters[endpointKey]
	if tb == nil || tb.rate != rate {
		if len(endpointRateLimiters) >= teamCooldownMaxSize {
			// Evict only an idle bucket; never split an active account's limiter.
			for key, candidate := range endpointRateLimiters {
				candidate.mu.Lock()
				idle := time.Since(candidate.lastUpdate) > time.Minute
				candidate.mu.Unlock()
				if idle {
					delete(endpointRateLimiters, key)
					break
				}
			}
			if len(endpointRateLimiters) >= teamCooldownMaxSize {
				endpointRateLimiterMu.Unlock()
				return fmt.Errorf("grok pacing registry capacity reached")
			}
		}
		tb = newTokenBucket(rate, 1)
		endpointRateLimiters[endpointKey] = tb
	}
	endpointRateLimiterMu.Unlock()
	if err := tb.wait(ctx); err != nil {
		return err
	}
	slog.Debug("Rate limiter: token acquired", "endpoint", endpointKey)
	return nil
}

func noteScopedRateLimit(ctx context.Context, provider, token, model string, status int, header http.Header, body []byte) *RateLimitMetadata {
	if status != http.StatusTooManyRequests {
		return nil
	}
	meta := RateLimitFromResponse(status, header, body)
	if meta == nil {
		meta = &RateLimitMetadata{Scope: RateLimitScopeRPM, RetryAfter: time.Minute}
	}
	if retry := parseRetryAfterHeader(header.Get("Retry-After"), time.Now()); retry > 0 {
		meta.RetryAfter = retry
	}
	meta.RetryAfter = accountpolicy.BoundRateLimitCooldown(meta.RetryAfter)
	identity := provider + ":" + rateLimitIdentity(ctx, token)
	for _, target := range util.UniqueStrings([]string{util.FirstNonEmpty(model, meta.Model, "*"), meta.Model}) {
		teamCooldown.Note(meta.Scope, identity, target, meta.RetryAfter)
		if meta.TeamID != "" {
			teamCooldown.Note(meta.Scope, provider+":team:"+meta.TeamID, target, meta.RetryAfter)
		}
	}
	recordTeamCooldownHit(meta)
	return meta
}
