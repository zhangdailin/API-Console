package web

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
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
	return "token:" + tokenCacheKey(token)
}

// webPlane identifies Web in the shared rate-limit state and metrics.
var webPlane = ratelimit.Plane{Provider: ProviderWeb, Namespace: "grok_web"}

// waitScopedRateLimit rejects an active team/account+model cooldown immediately,
// then consumes one pacing token. Request paths must rotate accounts instead of
// parking a request goroutine until an upstream 429 window expires.
func waitScopedRateLimit(ctx context.Context, provider, token, model string, rate float64) error {
	identity := provider + ":" + rateLimitIdentity(ctx, token)
	q, remaining, err := ratelimit.Active(ctx, identity, model)
	if err != nil {
		return err
	}
	if remaining > 0 {
		// Web classifies upstream failures from this text; keep its shape.
		return fmt.Errorf("grok upstream status=429 body=too_many_requests team %s model %s cooling down; retry-after=%s", q.Identity, q.Model, remaining.Round(time.Second))
	}
	return ratelimit.WaitPacing(ctx, identity, rate)
}

func noteScopedRateLimit(ctx context.Context, provider, token, model string, status int, header http.Header, body []byte) *ratelimit.Metadata {
	return ratelimit.Record(webPlane, provider+":"+rateLimitIdentity(ctx, token), model, status, header, body)
}
