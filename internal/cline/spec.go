package cline

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func init() { provider.Register(spec()) }

// spec describes the Cline channel to the rest of the gateway.
func spec() provider.Spec {
	return provider.Spec{
		ID:           channel.Cline,
		NewClient:    newProviderClient,
		Capabilities: capabilities{},
		Probe: &provider.Probe{
			Subject:  "Cline API",
			Endpoint: DefaultAPIBase,
			Run: func(ctx context.Context, cfg *config.Config) error {
				client := NewFromAccount(nil, cfg)
				defer client.Close()
				return client.ProbeReachability(ctx)
			},
		},
		Catalog:  catalogPort(),
		Admin:    adminPort(),
		Maintain: refreshCatalog,
	}
}

// newClientWithStore builds a client that writes a rotated credential back.
// WorkOS rotates the refresh token on every renewal, so a client without the
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

// SupportsModel checks the account's observed feed. An empty snapshot means
// "unknown", and an unknown catalog must not block a model: refusing it would
// break a channel whose snapshot has not synced yet.
func (capabilities) SupportsModel(acc *store.Account, model string) bool {
	if acc == nil || len(acc.ClineModelIDs) == 0 {
		return true
	}
	return CatalogSupportsModel(catalogRowsForPlan(acc.ClineModelIDs, acc.ClinePlan), model)
}

// IsFreeModel is always false: Cline has no free tier the gateway can route an
// exhausted account onto; every model is billed against the account.
func (capabilities) IsFreeModel(*store.Account, string) bool { return false }

// ContextWindows is empty: the feed names models but not their windows.
func (capabilities) ContextWindows(*store.Account) (map[string]int, map[string]int) {
	return nil, nil
}

func (capabilities) HonorsModelCooldown() bool { return false }

func catalogPort() *provider.Catalog {
	return &provider.Catalog{
		Source:                "cline_recommended_models",
		RequestTimeoutSeconds: 15,
		Observe: func(ctx context.Context, env provider.Env, acc *store.Account) (provider.Observation, error) {
			client := newClientWithStore(acc, env.Config, env.Store)
			defer client.Close()
			plan, planErr := client.FetchPlan(ctx)
			if planErr != nil && acc != nil {
				slog.Warn("Cline catalog plan read failed; keeping the recorded tier", "account_id", acc.ID, "error", planErr)
			} else if plan.Explicit {
				client.setPlan(plan.Name)
			}
			models, err := client.FetchUpstreamModels(ctx)
			record := provider.RecordSnapshot(CatalogSnapshot(models), setCatalogSnapshot)
			return provider.Observation{
				Models: CatalogModels(models),
				Record: func(account *store.Account, now time.Time) bool {
					if !record(account, now) {
						return false
					}
					if plan.Explicit {
						account.ClinePlan = plan.Name
					}
					return true
				},
			}, err
		},
		LastKnown: func(acc *store.Account) []provider.Model {
			if acc == nil {
				return nil
			}
			rows := catalogRowsForPlan(acc.ClineModelIDs, acc.ClinePlan)
			out := make([]provider.Model, 0, len(rows))
			for _, raw := range rows {
				var row catalogSnapshotRow
				if json.Unmarshal([]byte(raw), &row) != nil {
					continue
				}
				id := strings.TrimSpace(row.ID)
				if id == "" {
					continue
				}
				out = append(out, provider.Model{
					ID:            id,
					Name:          util.FirstNonEmpty(strings.TrimSpace(row.Name), id),
					Provider:      strings.TrimSpace(row.Provider),
					UpstreamModel: id,
					BillingTier:   row.BillingTier,
					BillingSource: "cline_recommended_models",
				})
			}
			return out
		},
		SetSnapshot: setCatalogSnapshot,
	}
}

func setCatalogSnapshot(acc *store.Account, rows []string, now time.Time) {
	acc.ClineModelIDs, acc.ClineModelsSyncedAt = rows, now
}

// CatalogModels maps the observed feed onto the channel's public model records.
//
// The public identifier is the upstream id: a client that saw it in /v1/models
// must be able to ask for it by that name. The display name is the one the feed
// published next to it, so the model list reads like the upstream's own catalog
// instead of repeating the identifier in both columns.
//
// Provider is the vendor half the feed names before the first "/". It is what
// the public list reports as owned_by, and it is the only distinction between
// the observed models of the same channel.
func CatalogModels(models []Model) []provider.Model {
	out := make([]provider.Model, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		out = append(out, provider.Model{
			ID:            id,
			Name:          util.FirstNonEmpty(strings.TrimSpace(model.Name), id),
			Provider:      strings.TrimSpace(model.Provider),
			UpstreamModel: id,
			BillingTier:   model.BillingTier,
			BillingSource: "cline_recommended_models",
		})
	}
	return out
}

// catalogRefreshDue reports whether the account's catalog snapshot should be
// re-read. The snapshot is an observation, and the feed changes without
// notice, so it is refreshed on the same cadence as the other providers.
func catalogRefreshDue(acc *store.Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	if len(acc.ClineModelIDs) == 0 || acc.ClineModelsSyncedAt.IsZero() {
		return true
	}
	return now.Sub(acc.ClineModelsSyncedAt) >= provider.HealthRefreshInterval
}

// refreshCatalog re-reads the model feed and subscription on the same cadence,
// so purchasing a plan does not leave a previously recorded free tier forever.
//
// It never publishes a compiled-in list: a failed read leaves the snapshot
// untouched, so an account that stopped being able to read the feed keeps the
// last observation instead of being silently widened.
func refreshCatalog(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil || !catalogRefreshDue(acc, time.Now()) {
		return
	}
	if !refreshqueue.WithLease(acc.ID, func() {
		client := newClientWithStore(acc, env.Config, env.Store)
		defer client.Close()
		catalogCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		observeAccount(catalogCtx, client, acc, "account_id", acc.ID,
			"Auto refresh cline catalog failed; keeping the last snapshot",
			"Auto refresh cline plan failed; keeping the recorded tier")
		cancel()
		if err := env.Store.UpdateAccount(ctx, acc); err != nil {
			slog.Warn("Auto refresh cline account: update failed", "account_id", acc.ID, "error", err)
		}
	}) {
		slog.Debug("Auto refresh cline catalog: account already refreshing", "account_id", acc.ID)
	}
}
