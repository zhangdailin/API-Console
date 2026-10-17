package refresh

import (
	"context"
	"log/slog"
	"sync"
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

const (
	maintenanceConcurrency = 4
	maintenanceTimeout     = time.Minute
)

// maintainAccounts hands every account to its own channel. The channel checks
// its catalog, quota and credential cadence before contacting the upstream, so
// this loop knows nothing about any of them. An account whose channel is not
// registered, or offers no maintenance, is left alone.
func maintainAccounts(ctx context.Context, env provider.Env, accounts []*store.Account) {
	runMaintenance(ctx, accounts, maintenanceConcurrency, func(ctx context.Context, acc *store.Account) {
		if spec, ok := provider.Lookup(acc.AccountType); ok && spec.Maintain != nil {
			spec.Maintain(ctx, env, acc)
		}
	})
}

// runMaintenance bounds upstream work and isolates slow or panicking accounts.
// A tick completes before another tick starts; channel refresh leases continue
// to serialize maintenance with administrative and request-time refreshes.
func runMaintenance(ctx context.Context, accounts []*store.Account, concurrency int, maintain func(context.Context, *store.Account)) {
	jobs := make(chan *store.Account)
	var workers sync.WaitGroup
	for range min(max(concurrency, 1), len(accounts)) {
		workers.Go(func() {
			for acc := range jobs {
				if ctx.Err() != nil {
					return
				}
				func() {
					accountCtx, cancel := context.WithTimeout(ctx, maintenanceTimeout)
					defer cancel()
					defer func() {
						if recover() != nil {
							slog.Error("Account maintenance panicked", "account_id", acc.ID)
						}
					}()
					maintain(accountCtx, acc)
				}()
			}
		})
	}
	seen := make(map[int64]struct{}, len(accounts))
	defer workers.Wait()
	defer close(jobs)
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if _, exists := seen[acc.ID]; exists {
			continue
		}
		seen[acc.ID] = struct{}{}
		select {
		case <-ctx.Done():
			return
		case jobs <- acc:
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
		refreshCtx := store.WithAccountChangeOrigin(ctx, store.AccountChangeOriginScheduler)
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
