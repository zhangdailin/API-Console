package warp

import (
	"context"
	"log/slog"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

func init() { provider.Register(spec()) }
func spec() provider.Spec {
	return provider.Spec{ID: channel.Warp, NewClient: func(acc *store.Account, cfg *config.Config, s *store.Store) provider.Client {
		return newClientWithStore(acc, cfg, s)
	}, ValidateRequest: validateRawRequest, Capabilities: capabilities{}, Selection: &provider.Selection{NeedsRefresh: selectionRefreshDue, Refresh: refreshSelectionCatalog}, Catalog: catalogPort(), Admin: adminPort(), Maintain: maintain, SingleGenerationAttempt: true, ExplicitResume: true, ExactModelID: true, SkipSharedCacheMarkers: true}
}
func newClientWithStore(acc *store.Account, cfg *config.Config, s *store.Store) *Client {
	c := NewFromAccount(acc, cfg)
	if s != nil {
		c.SetAccountStore(s)
		c.bindings.redis = redisBindings{store: s}
		c.bindings.keyBase = s.RedisPrefix()
	}
	return c
}

type capabilities struct{}

func (capabilities) SupportsModel(acc *store.Account, model string) bool {
	if !freshSnapshot(acc) || !validModel(model) {
		return false
	}
	for _, id := range acc.WarpModelIDs {
		if model == id {
			return true
		}
	}
	return false
}
func (capabilities) IsFreeModel(*store.Account, string) bool { return false }

// ContextWindows projects the input windows of the account's current catalog
// observation. A model that was never observed with a window is absent from
// the map rather than defaulted, so the shared handler can leave its budget
// unknown instead of inventing one.
func (capabilities) ContextWindows(acc *store.Account) (input, output map[string]int) {
	if acc == nil || len(acc.WarpModelWindows) == 0 {
		return nil, nil
	}
	windows := make(map[string]int, len(acc.WarpModelWindows))
	for id, window := range acc.WarpModelWindows {
		if validModel(id) && window > 0 {
			windows[id] = window
		}
	}
	if len(windows) == 0 {
		return nil, nil
	}
	return windows, nil
}

func (capabilities) HonorsModelCooldown() bool { return false }

// Maintenance reads only credentials and catalog, never a paid generation.
func maintain(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil {
		return
	}
	if !selectionRefreshDue(acc) && !quotaDue(acc, time.Now()) {
		return
	}
	refreshqueue.WithLease(acc.ID, func() {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if selectionRefreshDue(acc) {
			if err := refreshSelectionCatalog(ctx, env, acc); err != nil {
				slog.Warn("Warp account maintenance failed", "account_id", acc.ID, "error", err)
				return
			}
		}
		current, err := env.Store.GetAccount(ctx, acc.ID)
		if err != nil || current == nil {
			return
		}
		*acc = *current
		c := newClientWithStore(acc, env.Config, env.Store)
		if quotaDue(acc, time.Now()) {
			c.refreshQuota(ctx, acc, env.Store)
		}
	})
}
