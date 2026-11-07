package grok

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
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

// withRateLimitAccount records the account's rate-limit identity on ctx.
func withRateLimitAccount(ctx context.Context, acc *store.Account) context.Context {
	return grokbase.WithRateLimitAccount(ctx, acc)
}

func rateLimitIdentity(ctx context.Context, token string) string {
	if identity := grokbase.RateLimitAccount(ctx); identity != "" {
		return identity
	}
	return "token:" + credentialAffinity(token)
}

// buildPlane identifies Build in the shared rate-limit state and metrics.
var buildPlane = ratelimit.Plane{Provider: ProviderBuild, Namespace: "grok"}

// cooldownQueries lists the cooldown lookups for one Build account and model.
func cooldownQueries(ctx context.Context, acc *store.Account, model string) []ratelimit.Query {
	identity := ProviderBuild + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), acc.OAuthAccessToken)
	return ratelimit.Queries(identity, model)
}

// cooldownError is Build's error for an active cooldown: a synthetic 429 that
// routes like an upstream one.
func cooldownError(q ratelimit.Query, remaining time.Duration, err error) error {
	if err != nil {
		return err
	}
	if remaining > 0 {
		return newSyntheticCooldownError(q.Identity, q.Model, remaining)
	}
	return nil
}

// scopedRateLimitCooldown checks the shared cooldown without consuming a pacing
// token, so account selection and request dispatch use the same eligibility rule.
func scopedRateLimitCooldown(ctx context.Context, provider, token, model string) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return cooldownError(ratelimit.Active(ctx, provider+":"+rateLimitIdentity(ctx, token), model))
}

// waitScopedRateLimit rejects an active team/account+model cooldown immediately,
// then consumes one pacing token. Request paths must rotate accounts instead of
// parking a request goroutine until an upstream 429 window expires.
func waitScopedRateLimit(ctx context.Context, provider, token, model string, rate float64) error {
	if err := scopedRateLimitCooldown(ctx, provider, token, model); err != nil {
		return err
	}
	return ratelimit.WaitPacing(ctx, provider+":"+rateLimitIdentity(ctx, token), rate)
}

func noteScopedRateLimit(ctx context.Context, provider, token, model string, status int, header http.Header, body []byte) *ratelimit.Metadata {
	return ratelimit.Record(buildPlane, provider+":"+rateLimitIdentity(ctx, token), model, status, header, body)
}
