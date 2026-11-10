package doubao

import (
	"context"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

const observationTTL = 24 * time.Hour

func publishModels(ctx context.Context, env provider.Env) error {
	if env.Store == nil {
		return nil
	}
	accounts, err := env.Store.GetEnabledAccounts(ctx)
	if err != nil {
		return err
	}
	verified := false
	for _, acc := range accounts {
		if acc != nil && strings.EqualFold(acc.AccountType, "doubao") && strings.TrimSpace(acc.ClientCookie) != "" && !acc.DoubaoVerifiedAt.IsZero() && time.Since(acc.DoubaoVerifiedAt) < observationTTL {
			verified = true
			break
		}
	}
	rows := []*store.Model{}
	if verified {
		for index, model := range Models() {
			rows = append(rows, &store.Model{Channel: "doubao", ModelID: model.ID, Name: model.Name, Status: store.ModelStatusAvailable, Verified: true, SortOrder: index, Provider: "doubao-web", UpstreamModel: model.ModelItemKey, Capabilities: []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses}})
		}
	}
	_, err = env.Store.ReconcileDiscoveredModels(ctx, "doubao", rows, store.ModelReconcileOptions{Prune: true})
	return err
}

func PublishModels(ctx context.Context, cfg *config.Config, st *store.Store) error {
	return publishModels(ctx, provider.Env{Config: cfg, Store: st})
}
