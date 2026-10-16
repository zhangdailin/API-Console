package grok

import (
	"context"
	"log/slog"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"time"
)

type cooldownQuery struct {
	scope           RateLimitScope
	identity, model string
}

// Negative results live only for this selection. Dispatch always performs a
// fresh check so another replica's newly recorded cooldown remains effective.
type cooldownSnapshot struct{ deadlines map[cooldownQuery]time.Time }

func cooldownQueries(ctx context.Context, acc *store.Account, model string) []cooldownQuery {
	identity := ProviderBuild + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), acc.OAuthAccessToken)
	return identityCooldownQueries(identity, model)
}

func identityCooldownQueries(identity, model string) []cooldownQuery {
	queries := make([]cooldownQuery, 0, 4)
	for _, scope := range []RateLimitScope{RateLimitScopeRPS, RateLimitScopeRPM} {
		for _, target := range util.UniqueStrings([]string{model, "*"}) {
			queries = append(queries, cooldownQuery{scope, identity, target})
		}
	}
	return queries
}

func (s *cooldownSnapshot) prepare(ctx context.Context, queries []cooldownQuery) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if s.deadlines == nil {
		s.deadlines = make(map[cooldownQuery]time.Time)
	}
	missing := make([]cooldownQuery, 0, len(queries))
	seen := make(map[cooldownQuery]bool, len(queries))
	for _, q := range queries {
		if _, ok := s.deadlines[q]; !ok && !seen[q] {
			missing = append(missing, q)
			seen[q] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var shared map[cooldownQuery]time.Duration
	if backend := grokLimitsBackend(); backend != nil {
		var err error
		shared, err = backend.cooldownsRemaining(ctx, missing)
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err != nil {
			slog.Warn("grok cooldown batch unavailable; using process-local verdicts", "error", err)
		}
	}
	now := time.Now()
	for _, q := range missing {
		until := time.Time{}
		if remaining := shared[q]; remaining > 0 {
			until = now.Add(remaining)
		}
		s.deadlines[q] = until
	}
	return nil
}

func (s *cooldownSnapshot) check(ctx context.Context, queries []cooldownQuery) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	for _, q := range queries {
		remaining := teamCooldown.localRetryAfterFor(q.scope, q.identity, q.model)
		if shared := time.Until(s.deadlines[q]); shared > remaining {
			remaining = shared
		}
		if remaining > 0 {
			return newSyntheticCooldownError(q.identity, q.model, remaining)
		}
	}
	return nil
}
