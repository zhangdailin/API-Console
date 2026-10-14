package refresh

import (
	"context"
	"log/slog"
	"time"

	"orchids-api/internal/accountevents"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

// Hub is shared by the admin check path and the refresh scheduler.
var Hub = refreshqueue.Default()

// maintainAccounts hands every account to its own channel. The channel checks
// its catalog, quota and credential cadence before contacting the upstream, so
// this loop knows nothing about any of them. An account whose channel is not
// registered, or offers no maintenance, is left alone.
func maintainAccounts(ctx context.Context, env provider.Env, accounts []*store.Account) {
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if spec, ok := provider.Lookup(acc.AccountType); ok && spec.Maintain != nil {
			spec.Maintain(ctx, env, acc)
		}
	}
}

// StartTokenRefreshLoop refreshes account credentials on their own schedule.
func StartTokenRefreshLoop(ctx context.Context, configSnapshot func() *config.Config, s *store.Store, lb *loadbalancer.LoadBalancer) {
	interval := config.TokenRefreshInterval
	slog.Debug("Auto refresh token enabled", "interval", interval.String())

	refreshAccounts := func() {
		cfg := configSnapshot()
		if cfg == nil {
			return
		}
		refreshCtx := store.WithAccountChangeOrigin(context.Background(), store.AccountChangeOriginScheduler)
		accounts, err := s.GetEnabledAccounts(refreshCtx)
		if err != nil {
			slog.Error("Auto refresh token: list accounts failed", "error", err)
			return
		}
		maintainAccounts(refreshCtx, provider.Env{Config: cfg, Store: s}, accounts)
	}

	go func() {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic in token refresh loop", "error", err)
			}
		}()
		refreshAccounts()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshAccounts()
			case <-Kick.Channel():
				// An account was created, edited, deleted or rotated: the due set
				// changed, so waiting out the rest of the tick would be wrong. The
				// subscriber coalesces bursts, so a multi-field update wakes the
				// loop once.
				slog.Debug("Auto refresh token: account change detected; re-evaluating the due set")
				refreshAccounts()
			}
		}
	}()
}

// Kick wakes the refresh loop when an account changes, so a new account is
// picked up without waiting for the next tick.
var Kick = accountevents.NewFilteredKick(
	[]accountevents.Kind{accountevents.KindCreated, accountevents.KindCredential},
	store.AccountChangeOriginScheduler,
)
