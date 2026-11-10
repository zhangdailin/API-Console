package doubao

import (
	"context"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func init() { provider.Register(spec()) }

func spec() provider.Spec {
	return provider.Spec{
		ID: channel.Doubao,
		NewClient: func(acc *store.Account, cfg *config.Config, _ *store.Store) provider.Client {
			return NewFromAccount(acc, cfg)
		},
		Capabilities: capabilities{},
		Probe:        &provider.Probe{Subject: "Doubao Web", Endpoint: defaultBaseURL, Run: func(context.Context, *config.Config) error { return nil }},
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
			client := NewFromAccount(acc, env.Config)
			info, err := client.AccountInfo(ctx, client.jar())
			if err != nil {
				return provider.Observation{}, err
			}
			return provider.Observation{Record: func(target *store.Account, now time.Time) bool {
				if target == nil {
					return false
				}
				target.UserID = info.UserID
				if info.Name != "" {
					target.Name = info.Name
				}
				target.DoubaoVerifiedAt = now
				target.VerifiedAt = now
				return true
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
	client := NewFromAccount(acc, env.Config)
	info, err := client.AccountInfo(ctx, client.jar())
	now := time.Now()
	if err != nil {
		if strings.Contains(err.Error(), "status=401") {
			acc.StatusCode, acc.StatusMessage, acc.AuthStatus, acc.LastAttempt = "401", "Doubao cookie was rejected; scan again", store.AccountAuthStatusReauthRequired, now
			_ = env.Store.UpdateAccount(ctx, acc)
		}
		return
	}
	acc.UserID, acc.DoubaoVerifiedAt, acc.VerifiedAt = info.UserID, now, now
	if info.Name != "" {
		acc.Name = info.Name
	}
	_ = env.Store.UpdateAccount(ctx, acc)
}
