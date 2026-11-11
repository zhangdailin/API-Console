package doubao

import (
	"context"
	"strings"
	"time"

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
	rows := publishedModels(accounts, time.Now())
	_, err = env.Store.ReconcileDiscoveredModels(ctx, "doubao", rows, store.ModelReconcileOptions{Prune: true})
	return err
}

// publishedModels returns the routes a verified account supports, or nothing
// when no account has been verified inside the observation window. The channel
// serves Chat Completions only, so /messages and Responses are not advertised:
// offering them would promise a wire format the route table does not answer.
func publishedModels(accounts []*store.Account, now time.Time) []*store.Model {
	verified := false
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(acc.AccountType, "doubao") || strings.TrimSpace(acc.ClientCookie) == "" {
			continue
		}
		if !acc.DoubaoVerifiedAt.IsZero() && now.Sub(acc.DoubaoVerifiedAt) < observationTTL {
			verified = true
			break
		}
	}
	if !verified {
		return []*store.Model{}
	}
	rows := make([]*store.Model, 0, len(models))
	for index, model := range Models() {
		// Chat is the only wire protocol this channel routes, but the model
		// itself accepts images and files and can answer with generated images.
		// Those are chat-content capabilities, not extra endpoints, so clients
		// discover them here instead of probing /responses or /messages.
		rows = append(rows, &store.Model{Channel: "doubao", ModelID: model.ID, Name: model.Name, Status: store.ModelStatusAvailable, Verified: true, SortOrder: index, Provider: "doubao-web", UpstreamModel: model.ModelItemKey, Capabilities: []string{store.CapabilityChat, store.CapabilityImage}})
	}
	return rows
}
