package modelrefresh

import (
	"context"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/provider"
	_ "orchids-api/internal/provider/all"
	"orchids-api/internal/store"
)

// The helpers below keep the discovery tests written against one channel at a
// time, while the code under test reaches every channel through the port.

// fetchGrokBuildModelsForRefresh, when set, replaces the Build catalog read
// with a stub that returns profiles, which is the shape those tests describe.
var fetchGrokBuildModelsForRefresh func(ctx context.Context, cfg *config.Config, s *store.Store, acc *store.Account) ([]modelcatalog.Profile, error)

func init() {
	observe := observeCatalogForRefresh
	observeCatalogForRefresh = func(ctx context.Context, catalog *provider.Catalog, env provider.Env, acc *store.Account) (provider.Observation, error) {
		if fetch := fetchGrokBuildModelsForRefresh; fetch != nil && catalog.Kind == provider.CatalogAccountView {
			profiles, err := fetch(ctx, env.Config, env.Store, acc)
			return grok.CatalogObservation(profiles), err
		}
		return observe(ctx, catalog, env, acc)
	}
}

func discoverGrokModelsReport(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) (accountModelDiscoveryReport, error) {
	spec, _ := provider.Lookup("grok")
	return discoverAccountViewModels(ctx, cfg, s, spec, concurrency)
}

func accountCatalogSnapshotToDiscovered(channel string, acc *store.Account) []discoveredModel {
	spec, _ := provider.Lookup(channel)
	return toDiscovered(spec.Catalog.LastKnown(acc))
}

func clineCatalogToDiscovered(models []cline.Model) []discoveredModel {
	return toDiscovered(cline.CatalogModels(models))
}

// PersistAccountCatalogSnapshot stores a raw snapshot the way the background
// refresh of a row-snapshot channel does.
func PersistAccountCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, channel string, rows []string, updateFailureMessage string) {
	spec, ok := provider.Lookup(channel)
	if !ok {
		return
	}
	provider.PersistSnapshot(ctx, s, acc, spec.Catalog, rows, updateFailureMessage)
}
