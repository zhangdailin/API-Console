package modelpolicy

import (
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"strings"
)

// GrokAccountCatalog prefers upstream profiles over the legacy identifier projection.
func GrokAccountCatalog(acc *store.Account) []modelcatalog.Profile {
	if acc == nil {
		return nil
	}
	if len(acc.GrokModelCatalog) > 0 {
		return modelcatalog.CloneProfiles(acc.GrokModelCatalog)
	}
	out := make([]modelcatalog.Profile, 0, len(acc.GrokModels))
	for _, id := range acc.GrokModels {
		out = append(out, modelcatalog.Profile{ModelID: id})
	}
	return out
}

// GrokModelProfiles aggregates only authorized, enabled accounts bound to this route.
// Temporary concurrency and rate limits do not remove a catalog entitlement.
func GrokModelProfiles(model *store.Model, accounts []*store.Account) []modelcatalog.Profile {
	if model == nil {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(model.Provider), store.GrokProviderWeb) {
		return grokWebModelProfiles(model, accounts)
	}
	id := strings.TrimSpace(model.UpstreamModel)
	if id == "" {
		id = ExternalPublicID(model.ModelID)
	}
	catalogs := make([][]modelcatalog.Profile, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || !acc.Enabled || !accountpolicy.IsGrokAccount(acc) || !store.AccountAuthActive(acc) || strings.TrimSpace(acc.StatusCode) == "401" || !model.AllowsAccount(acc.ID) {
			continue
		}
		for _, profile := range GrokAccountCatalog(acc) {
			if strings.EqualFold(strings.TrimSpace(profile.ModelID), id) {
				catalogs = append(catalogs, []modelcatalog.Profile{profile})
				break
			}
		}
	}
	return modelcatalog.Aggregate(catalogs...)
}

// grokWebModelProfiles answers entitlement for a Grok Web route. grok.com has
// no per-account model catalog: the row was published from an active Web
// account's quota read, and it stays servable while such an account exists.
func grokWebModelProfiles(model *store.Model, accounts []*store.Account) []modelcatalog.Profile {
	for _, acc := range accounts {
		if acc == nil || !acc.Enabled || !acc.IsGrokWeb() || !store.AccountAuthActive(acc) || strings.TrimSpace(acc.StatusCode) == "401" || !model.AllowsAccount(acc.ID) {
			continue
		}
		return []modelcatalog.Profile{{ModelID: model.ModelID, Name: model.Name}}
	}
	return nil
}
