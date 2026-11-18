package doubao

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

func init() { provider.Register(spec()) }

func spec() provider.Spec {
	return provider.Spec{
		ID: channel.Doubao,
		NewClient: func(acc *store.Account, cfg *config.Config, s *store.Store) provider.Client {
			return newClientWithStore(acc, cfg, s)
		},
		Capabilities: capabilities{},
		Catalog:      catalogPort(), Admin: adminPort(), Maintain: maintain, PublishModels: publishModels,
	}
}

type capabilities struct{}

func (capabilities) SupportsModel(*store.Account, string) bool                      { return true }
func (capabilities) IsFreeModel(*store.Account, string) bool                        { return false }
func (capabilities) ContextWindows(*store.Account) (map[string]int, map[string]int) { return nil, nil }
func (capabilities) HonorsModelCooldown() bool                                      { return false }

func catalogPort() *provider.Catalog {
	return &provider.Catalog{Source: "doubao_web_verified", Kind: provider.CatalogAccountView,
		Eligible: func(acc *store.Account) bool { return acc != nil && strings.TrimSpace(acc.ClientCookie) != "" },
		Observe: func(ctx context.Context, env provider.Env, acc *store.Account) (provider.Observation, error) {
			client := newClientWithStore(acc, env.Config, env.Store)
			info, err := client.AccountInfo(ctx, client.jar())
			if err != nil {
				return provider.Observation{}, err
			}
			return provider.Observation{Record: func(target *store.Account, now time.Time) bool {
				return info.apply(target, now)
			}}, nil
		},
		LastKnown: func(acc *store.Account) []provider.Model {
			if acc == nil || acc.DoubaoVerifiedAt.IsZero() {
				return nil
			}
			return catalogModels()
		},
	}
}

func catalogModels() []provider.Model {
	rows := make([]provider.Model, 0, len(models))
	for _, model := range models {
		rows = append(rows, provider.Model{ID: model.ID, Name: model.Name, Provider: "doubao-web", UpstreamModel: model.ModelItemKey})
	}
	return rows
}

func maintain(ctx context.Context, env provider.Env, acc *store.Account) {
	if acc == nil || env.Store == nil || !acc.DoubaoVerifiedAt.IsZero() && time.Since(acc.DoubaoVerifiedAt) < provider.HealthRefreshInterval {
		return
	}
	refreshqueue.WithLease(acc.ID, func() { maintainSession(ctx, env, acc) })
}

func maintainSession(ctx context.Context, env provider.Env, acc *store.Account) {
	client := newClientWithStore(acc, env.Config, env.Store)
	info, err := client.AccountInfo(ctx, client.jar())
	now := time.Now()
	if err != nil {
		info.reject(acc, now, err)
		if acc.DoubaoSessionUpdate != nil {
			if saveErr := env.Store.UpdateAccount(ctx, acc); saveErr != nil {
				slog.Warn("Doubao session verdict not saved", "account_id", acc.ID)
			}
		}
		slog.Warn("Doubao session health check failed", "account_id", acc.ID)
		return
	}
	info.apply(acc, now)
	if err := env.Store.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("Doubao session observation not saved", "account_id", acc.ID)
		return
	}
	// Refresh the published routes too. Capabilities live on the model rows, so
	// a change to what the channel can do would otherwise stay invisible until
	// the next account verdict, which for a healthy account may never happen.
	_ = publishModels(ctx, env)
}
