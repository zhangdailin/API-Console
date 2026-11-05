package ratelimit

import (
	"context"
	"log/slog"
	"time"

	"orchids-api/internal/util"
)

// Query is one (scope, identity, model) cooldown lookup.
type Query struct {
	Scope           Scope
	Identity, Model string
}

// Snapshot caches one account selection's shared cooldown reads. Negative
// results live only for this selection; dispatch always performs a fresh check
// (Active) so another replica's newly recorded cooldown remains effective.
type Snapshot struct{ deadlines map[Query]time.Time }

// Queries lists the lookups that decide whether identity may call model: both
// windows, for the model and for the identity-wide "*" entry.
func Queries(identity, model string) []Query {
	queries := make([]Query, 0, 4)
	for _, scope := range []Scope{ScopeRPS, ScopeRPM} {
		for _, target := range util.UniqueStrings([]string{model, "*"}) {
			queries = append(queries, Query{scope, identity, target})
		}
	}
	return queries
}

// Prepare reads the shared cooldowns of queries not read yet, in one round trip.
func (s *Snapshot) Prepare(ctx context.Context, queries []Query) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if s.deadlines == nil {
		s.deadlines = make(map[Query]time.Time)
	}
	missing := make([]Query, 0, len(queries))
	seen := make(map[Query]bool, len(queries))
	for _, q := range queries {
		if _, ok := s.deadlines[q]; !ok && !seen[q] {
			missing = append(missing, q)
			seen[q] = true
		}
	}
	if len(missing) == 0 {
		return nil
	}
	var shared map[Query]time.Duration
	if backend := sharedBackend(); backend != nil {
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

// Check returns the first query still cooling down and its remaining time,
// combining the process registry with the shared deadlines Prepare read. The
// caller turns a hit into its plane's error.
func (s *Snapshot) Check(ctx context.Context, queries []Query) (Query, time.Duration, error) {
	if ctx.Err() != nil {
		return Query{}, 0, context.Cause(ctx)
	}
	for _, q := range queries {
		remaining := Cooldowns.LocalRetryAfterFor(q.Scope, q.Identity, q.Model)
		if shared := time.Until(s.deadlines[q]); shared > remaining {
			remaining = shared
		}
		if remaining > 0 {
			return q, remaining, nil
		}
	}
	return Query{}, 0, nil
}

// Active checks identity's cooldowns for model with a fresh snapshot, so a
// dispatch sees a cooldown another replica recorded since account selection.
func Active(ctx context.Context, identity, model string) (Query, time.Duration, error) {
	queries := Queries(identity, model)
	snapshot := &Snapshot{}
	if err := snapshot.Prepare(ctx, queries); err != nil {
		return Query{}, 0, err
	}
	return snapshot.Check(ctx, queries)
}
