package grok

import (
	"reflect"
	"slices"
	"strings"
	"time"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
)

// ProviderBuild is the only Grok plane this gateway serves: the Build (OAuth
// CLI) upstream. All retired provider planes have been removed.
const ProviderBuild = "build"

const modelSnapshotTTL = 6 * time.Hour

// ProviderForAccount reports the plane an account belongs to. Every Grok
// account is a Build account now, so legacy rows (and any stored provider value)
// normalise to Build.
func ProviderForAccount(acc *store.Account) string {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return ""
	}
	return ProviderBuild
}

func NormalizeProvider(acc *store.Account) bool {
	if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
		return false
	}
	provider := ProviderForAccount(acc)
	if strings.EqualFold(strings.TrimSpace(acc.GrokProvider), provider) {
		return false
	}
	acc.GrokProvider = provider
	return true
}

func AccountSupportsModel(acc *store.Account, modelID string) bool {
	if acc == nil {
		return false
	}
	modelID = strings.TrimSpace(modelID)
	if len(acc.GrokModelCatalog) > 0 {
		_, found := buildCatalogProfile(acc, modelID)
		return found
	}
	return slices.ContainsFunc(acc.GrokModels, func(model string) bool { return strings.EqualFold(strings.TrimSpace(model), modelID) })
}

func CLIModelsNeedSync(acc *store.Account, now time.Time) bool {
	if ProviderForAccount(acc) != ProviderBuild || len(acc.GrokModels) == 0 || acc.GrokModelsSyncedAt.IsZero() {
		return true
	}
	return !now.Before(acc.GrokModelsSyncedAt.Add(modelSnapshotTTL))
}

// ApplyCLIModelCatalog atomically projects one successful upstream catalog onto
// the account's identifier compatibility field and durable profile field.
func ApplyCLIModelCatalog(acc *store.Account, catalog []modelcatalog.Profile, now time.Time) bool {
	if acc == nil {
		return false
	}
	catalog = modelcatalog.Aggregate(catalog)
	models := modelcatalog.ModelIDs(catalog)
	normalized := models
	if len(normalized) == 0 {
		return false
	}
	changed := !slices.EqualFunc(acc.GrokModels, normalized, strings.EqualFold) || !reflect.DeepEqual(acc.GrokModelCatalog, catalog)
	acc.GrokModels = normalized
	acc.GrokModelCatalog = modelcatalog.CloneProfiles(catalog)
	acc.GrokModelsSyncedAt = now.UTC()
	return changed
}
