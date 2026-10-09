package modelrefresh

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// fetchGrokBuildModelsForRefresh reads the official Build CLI catalog.  It is
// deliberately kept as an injectable control-plane operation: model refresh
// must never send a completion simply to discover an account's capabilities.
var fetchGrokBuildModelsForRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, acc *store.Account) ([]modelcatalog.Profile, error) {
	client := grok.NewCLIClient(cfg)
	client.SetAccountStore(s)
	return client.FetchModelCatalog(ctx, acc)
}

type grokBuildModelDiscovery struct {
	account *store.Account
	catalog []modelcatalog.Profile
	err     error
}

func discoverGrokModelsReport(ctx context.Context, cfg *config.Config, s *store.Store, concurrency int) (accountModelDiscoveryReport, error) {
	report := accountModelDiscoveryReport{}
	accounts, err := grokBuildModelDiscoveryAccounts(ctx, s)
	if err != nil {
		return report, err
	}
	if len(accounts) == 0 {
		return report, &noActiveAccountsError{Channel: "Grok"}
	}

	ordered := make([]grokBuildModelDiscovery, len(accounts))
	taskErrors := runIndexedModelRefreshWorkers(len(accounts), concurrency, func(index int) {
		acc := accounts[index]
		ordered[index].account = acc
		catalog, fetchErr := fetchGrokBuildModelsForRefresh(ctx, cfg, s, acc)
		ordered[index] = grokBuildModelDiscovery{account: acc, catalog: catalog, err: fetchErr}
	})

	for index, taskErr := range taskErrors {
		if taskErr != nil {
			ordered[index].err = taskErr
		}
	}
	now := time.Now().UTC()
	seen := make(map[string]struct{})
	for _, result := range ordered {
		attempt := accountModelDiscoveryAttempt{Err: result.err}
		if result.account != nil {
			attempt.AccountID = result.account.ID
		}
		if result.account == nil {
			attempt.Err = fmt.Errorf("missing Grok Build account")
			report.Attempts = append(report.Attempts, attempt)
			continue
		}
		if result.err == nil && len(result.catalog) > 0 {
			grok.NormalizeProvider(result.account)
			grok.ApplyCLIModelCatalog(result.account, result.catalog, now)
			if updateErr := s.UpdateAccount(ctx, result.account); updateErr != nil {
				attempt.Err = fmt.Errorf("persist grok build model catalog: %w", updateErr)
			}
		} else {
			// A failed account contributes its last-known-good union, but its Err
			// prevents negative reconciliation for the entire round.
			attempt.UsedLastKnownGood = len(result.account.GrokModels) > 0
		}
		for _, rawID := range result.account.GrokModels {
			id := canonicalGrokRefreshModelID(rawID)
			if id == "" {
				continue
			}
			spec, ok := grok.ResolveModel(id)
			if !ok {
				spec = grok.ModelSpec{ID: id, Name: id, UpstreamModel: id, Upstream: grok.UpstreamCLI}
			}
			candidate := discoveredModel{ID: spec.ID, Name: util.FirstNonEmpty(spec.Name, spec.ID), Verified: true}
			attempt.Candidates = append(attempt.Candidates, candidate)
			key := strings.ToLower(candidate.ID)
			if _, exists := seen[key]; !exists {
				seen[key] = struct{}{}
				candidate.SortOrder = len(report.Candidates)
				report.Candidates = append(report.Candidates, candidate)
			}
		}
		report.Attempts = append(report.Attempts, attempt)
	}
	succeeded, _ := report.counts()
	if succeeded == 0 {
		return report, fmt.Errorf("official Grok Build model discovery failed for all enabled OAuth accounts")
	}
	report.Source = "grok_build_models"
	return report, nil
}

func canonicalGrokRefreshModelID(modelID string) string {
	id := strings.TrimSpace(modelID)
	if id == "" {
		return ""
	}
	// Media-generation products are not exposed by the Build-only gateway.
	if strings.Contains(strings.ToLower(id), "imagine") || strings.Contains(strings.ToLower(id), "voice") || strings.HasPrefix(strings.ToLower(id), "grok-"+"stt") {
		return ""
	}
	if spec, ok := grok.ResolveModel(id); ok {
		return spec.ID
	}
	return strings.ToLower(id)
}

func grokBuildModelDiscoveryAccounts(ctx context.Context, s *store.Store) ([]*store.Account, error) {
	if s == nil {
		return nil, fmt.Errorf("store not configured")
	}
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || grok.ProviderForAccount(acc) != grok.ProviderBuild || !strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth") {
			continue
		}
		if strings.TrimSpace(acc.OAuthAccessToken) == "" && strings.TrimSpace(acc.OAuthRefreshToken) == "" {
			continue
		}
		out = append(out, acc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
