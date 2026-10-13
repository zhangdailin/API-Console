package workbuddy

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func init() { provider.Register(spec()) }

// spec describes the WorkBuddy channel to the rest of the gateway.
func spec() provider.Spec {
	return provider.Spec{
		ID:           channel.WorkBuddy,
		NewClient:    newProviderClient,
		Capabilities: capabilities{},
		Probe: &provider.Probe{
			Subject:  "WorkBuddy backend",
			Endpoint: DefaultBaseURL,
			Run: func(ctx context.Context, cfg *config.Config) error {
				client := NewFromAccount(nil, cfg)
				defer client.Close()
				return client.ProbeReachability(ctx)
			},
		},
		Catalog:  catalogPort(),
		Admin:    adminPort(),
		Maintain: maintain,
	}
}

// newClientWithStore builds a client that writes a rotated credential back.
// Keycloak rotates the refresh token on every renewal, so a client without the
// store would keep using a credential the pool has already replaced.
func newClientWithStore(acc *store.Account, cfg *config.Config, s *store.Store) *Client {
	client := NewFromAccount(acc, cfg)
	if s != nil {
		client.SetAccountStore(s)
	}
	return client
}

func newProviderClient(acc *store.Account, cfg *config.Config, s *store.Store) provider.Client {
	return newClientWithStore(acc, cfg, s)
}

type capabilities struct{}

// SupportsModel is always true: the channel refuses an unknown model itself.
func (capabilities) SupportsModel(*store.Account, string) bool { return true }

func (capabilities) IsFreeModel(acc *store.Account, model string) bool {
	if acc == nil {
		return false
	}
	return IsFreeModelInCatalog(acc.WorkBuddyModelIDs, model)
}

func (capabilities) ContextWindows(acc *store.Account) (map[string]int, map[string]int) {
	if acc == nil {
		return nil, nil
	}
	return CatalogContextWindows(acc.WorkBuddyModelIDs)
}

func (capabilities) HonorsModelCooldown() bool { return true }

func catalogPort() *provider.Catalog {
	return &provider.Catalog{
		Source:                "workbuddy_cli_models",
		RequestTimeoutSeconds: 15,
		Observe: func(ctx context.Context, env provider.Env, acc *store.Account) (provider.Observation, error) {
			client := newClientWithStore(acc, env.Config, env.Store)
			defer client.Close()
			models, err := client.FetchModels(ctx)
			return provider.Observation{
				Models: catalogModels(models),
				Record: provider.RecordSnapshot(CatalogSnapshot(models), setCatalogSnapshot),
			}, err
		},
		LastKnown: func(acc *store.Account) []provider.Model {
			if acc == nil {
				return nil
			}
			rows := provider.ParseSnapshotRows(acc.WorkBuddyModelIDs)
			out := make([]provider.Model, 0, len(rows))
			for _, row := range rows {
				id := strings.TrimSpace(row.ID)
				if id == "" {
					continue
				}
				out = append(out, provider.Model{
					ID:   id,
					Name: util.FirstNonEmpty(strings.TrimSpace(row.Name), strings.TrimSpace(row.DisplayName), id),
				})
			}
			return out
		},
		SetSnapshot: setCatalogSnapshot,
	}
}

func setCatalogSnapshot(acc *store.Account, rows []string, now time.Time) {
	acc.WorkBuddyModelIDs, acc.WorkBuddyModelsSyncedAt = rows, now
}

func catalogModels(models []WorkBuddyModel) []provider.Model {
	out := make([]provider.Model, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		out = append(out, provider.Model{ID: id, Name: util.FirstNonEmpty(strings.TrimSpace(model.Name), id)})
	}
	return out
}

// maintain keeps one account's catalog and credit meter current.
func maintain(ctx context.Context, env provider.Env, acc *store.Account) {
	refreshCatalog(ctx, env, acc)
	refreshQuota(ctx, env, acc)
}

func catalogRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if len(acc.WorkBuddyModelIDs) == 0 || acc.WorkBuddyModelsSyncedAt.IsZero() {
		return true
	}
	return now.Sub(acc.WorkBuddyModelsSyncedAt) >= provider.HealthRefreshInterval
}

// refreshCatalog keeps the account-scoped CLI whitelist current without
// replacing a usable last-known-good snapshot when the control plane is down.
func refreshCatalog(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil || !catalogRefreshDue(acc, time.Now()) {
		return
	}
	if !refreshqueue.WithLease(acc.ID, func() {
		client := newClientWithStore(acc, env.Config, env.Store)
		defer client.Close()
		catalogCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		models, err := client.FetchModels(catalogCtx)
		cancel()
		if err != nil {
			slog.Warn("Auto refresh workbuddy catalog failed; keeping the last snapshot", "account_id", acc.ID, "error", err)
			return
		}
		provider.PersistSnapshot(ctx, env.Store, acc, catalogPort(), CatalogSnapshot(models), "Auto refresh workbuddy catalog: update account failed")
	}) {
		slog.Debug("Auto refresh workbuddy catalog: account already refreshing", "account_id", acc.ID)
	}
}

// quotaRefreshDue reports whether the account's credit-meter reading should be
// re-read on this tick.
func quotaRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if acc.WorkBuddyQuota.SyncedAt.IsZero() {
		return true
	}
	return now.Sub(acc.WorkBuddyQuota.SyncedAt) >= provider.HealthRefreshInterval
}

// refreshQuota re-reads the credit meter the account table, the
// exhausted/free-only verdict and the selection filter are all built from.
//
// Before this existed the reading was only written at login and on a manual
// check, so it aged without bound: production held a 36-hour-old snapshot for an
// account whose live balance had already moved, and an operator comparing the
// table with the upstream was comparing two different moments.
//
// The verdict follows the same rule as the manual check: a spent metered package
// narrows the account to its confirmed free catalog instead of parking it, and a
// reading that shows credits again restores full capability.
func refreshQuota(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil || !quotaRefreshDue(acc, time.Now()) {
		return
	}
	if !refreshqueue.WithLease(acc.ID, func() {
		// The meter read renews an expired access token first, and that renewal
		// rotates the refresh token, so this client needs the store as well.
		client := newClientWithStore(acc, env.Config, env.Store)
		defer client.Close()
		quotaCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		quota, err := client.FetchQuota(quotaCtx)
		cancel()
		if err != nil {
			slog.Warn("Auto refresh workbuddy quota failed; keeping the last reading", "account_id", acc.ID, "error", err)
			return
		}
		ApplyQuota(acc, quota)
		switch {
		case acc.UsageLimit > 0 && acc.UsageCurrent <= 0:
			accountpolicy.Verdict{
				Status:  store.AccountStatusWorkBuddyQuotaExhausted,
				Message: "WorkBuddy allowance exhausted; free catalog models remain eligible",
				Scope:   accountpolicy.ScopeAccount,
				At:      time.Now(),
			}.Apply(acc)
		case acc.StatusCode == store.AccountStatusWorkBuddyQuotaExhausted:
			// The meter shows credits again, so the free-only narrowing is over.
			// Only that verdict is cleared: another status (a refused credential, a
			// throttle) is not something a credit reading can speak to.
			acc.StatusCode = ""
			acc.StatusMessage = ""
			acc.LastAttempt = time.Time{}
			acc.VerifiedAt = time.Now()
		}
		if err := env.Store.UpdateAccount(ctx, acc); err != nil {
			slog.Warn("Auto refresh workbuddy quota: update account failed", "account_id", acc.ID, "error", err)
		}
	}) {
		slog.Debug("Auto refresh workbuddy quota: account already refreshing", "account_id", acc.ID)
	}
}
