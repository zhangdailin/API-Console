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
	if provider == "" || strings.EqualFold(strings.TrimSpace(acc.GrokProvider), provider) {
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
	// Do not block a just-authorized account before its first non-billable
	// catalog sync. Once observed, the catalog is authoritative.
	if len(acc.GrokModels) == 0 {
		return true
	}
	for _, model := range acc.GrokModels {
		if strings.EqualFold(strings.TrimSpace(model), modelID) {
			return true
		}
	}
	return false
}

const (
	buildGrok45Model   = "grok-4.5"
	buildGrok46Model   = "grok-4.6"
	buildComposerModel = "grok-composer-2.5-fast"
)

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
	seen := make(map[string]struct{}, len(models)+3)
	normalized := make([]string, 0, len(models)+3)
	appendModel := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		normalized = append(normalized, model)
	}
	for _, model := range models {
		appendModel(model)
	}

	// grok2api's NormalizeAccountModelCapabilities, restored. The upstream's own
	// catalog is authoritative for what an account can serve, but two entries are
	// derived from the account's tier and credential rather than listed: a Build
	// account advertising 4.6 can always serve 4.5, and an OAuth Build account can
	// always serve Composer. Without them the catalog omits models that route
	// perfectly well.
	hasGrok46 := false
	for _, model := range normalized {
		if strings.EqualFold(strings.TrimSpace(model), buildGrok46Model) {
			hasGrok46 = true
		}
	}
	if hasGrok46 {
		appendModel(buildGrok45Model)
	}
	if ProviderForAccount(acc) == ProviderBuild && strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
		appendModel(buildComposerModel)
	}
	if len(normalized) == 0 {
		return false
	}
	changed := !slices.EqualFunc(acc.GrokModels, normalized, strings.EqualFold) || !reflect.DeepEqual(acc.GrokModelCatalog, catalog)
	acc.GrokModels = normalized
	acc.GrokModelCatalog = modelcatalog.CloneProfiles(catalog)
	acc.GrokModelsSyncedAt = now.UTC()
	return changed
}
