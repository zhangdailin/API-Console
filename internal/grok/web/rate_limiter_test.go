package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestWebCooldownKeepsItsErrorTextAndPlane(t *testing.T) {
	previous := ratelimit.Cooldowns
	ratelimit.Cooldowns = ratelimit.NewRegistry()
	t.Cleanup(func() { ratelimit.Cooldowns = previous })

	ctx := withRateLimitAccount(context.Background(), &store.Account{ID: 7, TeamID: "web-team"})
	meta := noteScopedRateLimit(ctx, ProviderWeb, "token", "grok-4", http.StatusTooManyRequests, http.Header{"Retry-After": {"30"}}, nil)
	testutil.True(t, meta != nil, "a 429 must be recorded")

	err := waitScopedRateLimit(ctx, ProviderWeb, "token", "grok-4", 0)
	testutil.Error(t, err)
	// Web's upstream classifier reads this text; the shape must not drift.
	testutil.True(t, strings.HasPrefix(err.Error(), "grok upstream status=429 body=too_many_requests team web:team:web-team model grok-4 cooling down; retry-after="), err.Error())

	// The registry is shared with Build, but a Web cooldown never blocks Build.
	remaining := ratelimit.Cooldowns.RetryAfterFor(ratelimit.ScopeRPM, "build:team:web-team", "grok-4")
	testutil.Equal(t, remaining, time.Duration(0))
}

func TestWebNon429IsNotRecorded(t *testing.T) {
	testutil.True(t, noteScopedRateLimit(context.Background(), ProviderWeb, "token", "grok-4", http.StatusOK, http.Header{}, nil) == nil, "only 429 records a cooldown")
}
