package modelrefresh

import (
	"context"
	"fmt"
	"strings"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func syncModelsForChannelConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*Result, error) {
	channel = normalizeAdminModelChannel(channel)
	if channel == "" {
		return nil, fmt.Errorf("channel is required")
	}
	if s == nil {
		return nil, fmt.Errorf("store not configured")
	}

	concurrency = normalizeModelRefreshConcurrency(concurrency)
	report, err := discoverModelsForChannelReport(ctx, cfg, s, channel, concurrency)
	if err != nil {
		return nil, err
	}
	if len(report.Candidates) == 0 {
		return nil, fmt.Errorf("%s has no discoverable models", channel)
	}

	succeeded, failed := report.counts()
	allowPrune := failed == 0
	result, err := applyModelRefreshWithPrune(ctx, s, channel, report.Source, report.Candidates, allowPrune)
	if result != nil {
		result.Concurrency = concurrency
		result.AccountsTotal = len(report.Attempts)
		result.AccountsSuccess = succeeded
		result.AccountsFailed = failed
		result.Partial = succeeded > 0 && failed > 0
		result.KeptLastKnownGood = result.Partial
		switch {
		case result.Partial:
			result.Outcome = "partial"
		case succeeded > 0:
			result.Outcome = "success"
		}
	}
	return result, err
}

func normalizeAdminModelChannel(value string) string {
	id, ok := channel.Parse(value)
	if !ok {
		return ""
	}
	definition, _ := channel.DefinitionFor(id)
	return definition.Label
}

func applyModelRefreshWithPrune(ctx context.Context, s *store.Store, channel string, source string, candidates []discoveredModel, allowPrune bool) (*Result, error) {
	// The single gate that keeps locally compiled-in or cached catalogs out of
	// model management. Discovery is expected to fail instead of returning a
	// non-upstream source; this refuses the write if it ever does.
	if !isUpstreamCatalogSource(source) {
		return nil, fmt.Errorf("%s refresh refused: %q is not an upstream catalog source", channel, source)
	}

	existingModels, err := s.ListModels(ctx)
	if err != nil {
		return nil, err
	}

	result := &Result{
		Channel:    channel,
		Source:     source,
		Discovered: len(candidates),
	}

	existingByID := make(map[string]*store.Model)
	fetchedSet := make(map[string]discoveredModel, len(candidates))
	for _, model := range candidates {
		fetchedSet[model.ID] = model
		if model.Verified {
			result.Verified++
		}
	}

	for _, model := range existingModels {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), channel) {
			continue
		}
		existingByID[model.ModelID] = model
	}

	defaultModelID := chooseRefreshedDefaultModel(existingByID, candidates)
	result.DefaultModelID = defaultModelID

	// The channel's catalog port says how its rows are shaped and scoped. A
	// channel without one publishes plain rows.
	var catalog *provider.Catalog
	if spec, ok := provider.Lookup(channel); ok {
		catalog = spec.Catalog
	}
	records := make([]*store.Model, 0, len(candidates))
	for _, model := range candidates {
		record := &store.Model{
			Channel: channel, ModelID: model.ID, Name: util.FirstNonEmpty(model.Name, model.ID),
			Status: store.ModelStatusAvailable, Verified: model.Verified, IsDefault: model.ID == defaultModelID,
			SortOrder: model.SortOrder, Provider: model.Provider, UpstreamModel: model.UpstreamModel,
			BillingTier: strings.ToLower(strings.TrimSpace(model.BillingTier)), BillingSource: strings.ToLower(strings.TrimSpace(model.BillingSource)),
			Origin: "discovery",
		}
		if catalog != nil && catalog.ShapeRecord != nil {
			catalog.ShapeRecord(record)
		}
		records = append(records, record)
	}
	reconcileOptions := store.ModelReconcileOptions{
		Prune: allowPrune && shouldDeleteMissingModelsOnRefresh(source),
	}
	if catalog != nil && catalog.ProviderScope != "" && source == catalog.Source && allowPrune {
		reconcileOptions.Prune = true
		reconcileOptions.ProviderScope = catalog.ProviderScope
	}
	applied, err := s.ReconcileDiscoveredModels(ctx, channel, records, reconcileOptions)
	if err != nil {
		return nil, err
	}
	result.Added, result.Updated, result.Deleted = applied.Added, applied.Updated, applied.Deleted
	result.AddedModelIDs, result.DeletedModelIDs = applied.AddedModelIDs, applied.DeletedModelIDs
	return result, nil
}

// shouldDeleteMissingModelsOnRefresh reports whether a whole-channel catalog is
// authoritative. Grok Build is handled as a provider-scoped catalog by apply;
// it must remain false here so it can never prune retired provider planes.
func shouldDeleteMissingModelsOnRefresh(source string) bool {
	source = strings.TrimSpace(source)
	if source == "grok_build_models" || source == "workbuddy_cli_models" {
		// Grok is a provider subset; catalog reads may be inconclusive because of
		// quota/transport; WorkBuddy can return a degraded whitelist fallback.
		// Absence from any of these is not authoritative deletion evidence.
		return false
	}
	return isUpstreamCatalogSource(source)
}

func chooseRefreshedDefaultModel(existing map[string]*store.Model, ordered []discoveredModel) string {
	for _, model := range ordered {
		if current := existing[model.ID]; current != nil && current.IsDefault {
			return model.ID
		}
	}
	for _, model := range ordered {
		return model.ID
	}
	return ""
}
