package modelrefresh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// observeCatalogForRefresh is the upstream read of one account's catalog. It is
// a variable so a test can replace the network. It is deliberately a
// control-plane read: model refresh must never send a completion simply to
// discover an account's capabilities.
var observeCatalogForRefresh = func(ctx context.Context, catalog *provider.Catalog, env provider.Env, acc *store.Account) (provider.Observation, error) {
	return catalog.Observe(ctx, env, acc)
}

// discoverModelsForChannelReport reads a channel's catalog through the port it
// registered. The two shapes differ in where the published rows come from, not
// in which channel is asking.
func discoverModelsForChannelReport(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (accountModelDiscoveryReport, error) {
	spec, ok := provider.Lookup(channel)
	if !ok || spec.Catalog == nil {
		return accountModelDiscoveryReport{}, fmt.Errorf("unsupported channel: %s", channel)
	}
	if spec.Catalog.Kind == provider.CatalogAccountView {
		return discoverAccountViewModels(ctx, cfg, s, spec, concurrency)
	}
	return discoverAccountCatalogModels(ctx, cfg, s, channel, concurrency)
}

// toDiscovered maps a channel's catalog entries onto refresh candidates. Every
// entry is an observation the channel just made or a row it stored earlier, so
// each one is marked verified.
func toDiscovered(models []provider.Model) []discoveredModel {
	out := make([]discoveredModel, 0, len(models))
	for i, model := range models {
		out = append(out, discoveredModel{
			ID: model.ID, Name: model.Name, SortOrder: i, Verified: true,
			Provider: model.Provider, UpstreamModel: model.UpstreamModel,
			BillingTier: model.BillingTier, BillingSource: model.BillingSource,
		})
	}
	return out
}

// persistObservation records one successful catalog read on the account so
// model selection resolves against the same list the channel publishes. An
// observation that records nothing leaves the stored snapshot alone.
func persistObservation(ctx context.Context, s *store.Store, acc *store.Account, observation provider.Observation, now time.Time) error {
	if acc == nil || acc.ID == 0 || observation.Record == nil || !observation.Record(acc, now) {
		return nil
	}
	return s.UpdateAccount(ctx, acc)
}

// discoverAccountCatalogModels reads a row-snapshot catalog from every enabled
// account of the channel. An account whose read fails contributes its last
// known good snapshot, and its error keeps the round from pruning.
func discoverAccountCatalogModels(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (accountModelDiscoveryReport, error) {
	channel = normalizeAdminModelChannel(channel)
	accountType := strings.ToLower(channel)
	spec, ok := provider.Lookup(accountType)
	if !ok || spec.Catalog == nil || spec.Catalog.Kind != provider.CatalogRows {
		return accountModelDiscoveryReport{}, fmt.Errorf("unsupported account catalog channel: %s", channel)
	}
	catalog := spec.Catalog
	accounts, err := catalogAccounts(ctx, s, accountType, catalog)
	if err != nil {
		return accountModelDiscoveryReport{}, fmt.Errorf("%s model discovery failed: %w", accountType, err)
	}
	if len(accounts) == 0 {
		return accountModelDiscoveryReport{}, &noActiveAccountsError{Channel: channel}
	}

	env := provider.Env{Config: refreshModelRequestConfig(cfg, accountType), Store: s}
	report := accountModelDiscoveryReport{Source: catalog.Source, Attempts: make([]accountModelDiscoveryAttempt, len(accounts))}
	taskErrors := runIndexedModelRefreshWorkers(len(accounts), concurrency, func(index int) {
		acc := accounts[index]
		attempt := accountModelDiscoveryAttempt{AccountID: acc.ID}
		observation, fetchErr := observeCatalogForRefresh(ctx, catalog, env, acc)
		attempt.Err = fetchErr
		attempt.Candidates = toDiscovered(observation.Models)
		if fetchErr == nil && len(attempt.Candidates) > 0 {
			// A snapshot that cannot be saved does not fail the attempt: the
			// catalog was observed, and the next refresh writes it again.
			if updateErr := persistObservation(ctx, s, acc, observation, time.Now()); updateErr != nil {
				slog.Warn("failed to persist "+accountType+" model snapshot", "account_id", acc.ID, "error", updateErr)
			}
		}
		if attempt.Err == nil && len(attempt.Candidates) == 0 {
			attempt.Err = fmt.Errorf("%s account #%d returned an empty upstream catalog", accountType, acc.ID)
		}
		if attempt.Err != nil {
			attempt.Candidates = toDiscovered(catalog.LastKnown(acc))
			attempt.UsedLastKnownGood = len(attempt.Candidates) > 0
		}
		report.Attempts[index] = attempt
	})

	for index, taskErr := range taskErrors {
		if taskErr != nil {
			report.Attempts[index] = accountModelDiscoveryAttempt{AccountID: accounts[index].ID, Err: taskErr}
		}
	}

	report.Candidates = unionAccountCatalogAttempts(report.Attempts)
	succeeded, _ := report.counts()
	if succeeded == 0 {
		var causes []error
		for _, attempt := range report.Attempts {
			if attempt.Err != nil {
				causes = append(causes, attempt.Err)
			}
		}
		return accountModelDiscoveryReport{}, fmt.Errorf("%s model discovery failed: %w", accountType, errors.Join(causes...))
	}
	return report, nil
}

// discoverAccountViewModels reads an account-view catalog. A successful read is
// recorded on the account first, and the published rows are then whatever the
// account stores, so a row can never differ from what selection resolves
// against.
func discoverAccountViewModels(ctx context.Context, cfg *config.Config, s *store.Store, spec provider.Spec, concurrency int) (accountModelDiscoveryReport, error) {
	report := accountModelDiscoveryReport{}
	if s == nil {
		return report, fmt.Errorf("store not configured")
	}
	catalog := spec.Catalog
	label := normalizeAdminModelChannel(string(spec.ID))
	accounts, err := catalogAccounts(ctx, s, string(spec.ID), catalog)
	if err != nil {
		return report, err
	}
	if len(accounts) == 0 {
		return report, &noActiveAccountsError{Channel: label}
	}
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })

	type accountObservation struct {
		account     *store.Account
		observation provider.Observation
		err         error
	}
	env := provider.Env{Config: cfg, Store: s}
	ordered := make([]accountObservation, len(accounts))
	taskErrors := runIndexedModelRefreshWorkers(len(accounts), concurrency, func(index int) {
		acc := accounts[index]
		ordered[index].account = acc
		observation, fetchErr := observeCatalogForRefresh(ctx, catalog, env, acc)
		ordered[index] = accountObservation{account: acc, observation: observation, err: fetchErr}
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
		if result.account == nil {
			attempt.Err = fmt.Errorf("missing %s account", label)
			report.Attempts = append(report.Attempts, attempt)
			continue
		}
		attempt.AccountID = result.account.ID
		recorded := result.err == nil && result.observation.Record != nil && result.observation.Record(result.account, now)
		if recorded {
			if updateErr := s.UpdateAccount(ctx, result.account); updateErr != nil {
				attempt.Err = fmt.Errorf("persist %s model catalog: %w", spec.ID, updateErr)
			}
		}
		// A failed account contributes its last-known-good union, but its Err
		// prevents negative reconciliation for the entire round.
		attempt.Candidates = toDiscovered(catalog.LastKnown(result.account))
		for i := range attempt.Candidates {
			attempt.Candidates[i].SortOrder = 0
		}
		if !recorded {
			attempt.UsedLastKnownGood = len(attempt.Candidates) > 0
		}
		for _, candidate := range attempt.Candidates {
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
		return report, fmt.Errorf("%s model discovery failed for every eligible account", label)
	}
	report.Source = catalog.Source
	return report, nil
}

func unionAccountCatalogAttempts(attempts []accountModelDiscoveryAttempt) []discoveredModel {
	out := make([]discoveredModel, 0)
	seen := make(map[string]int)
	for _, attempt := range attempts {
		for _, candidate := range attempt.Candidates {
			key := strings.ToLower(strings.TrimSpace(candidate.ID))
			if key == "" {
				continue
			}
			if index, ok := seen[key]; ok {
				existing := &out[index]
				if existing.Name == "" {
					existing.Name = candidate.Name
				}
				if existing.Provider == "" {
					existing.Provider = candidate.Provider
				}
				if existing.UpstreamModel == "" {
					existing.UpstreamModel = candidate.UpstreamModel
				}
				continue
			}
			candidate.SortOrder = len(out)
			seen[key] = len(out)
			out = append(out, candidate)
		}
	}
	return out
}

// refreshModelRequestConfig returns the configuration a manual discovery read
// runs with: a copy whose request timeout is capped by the channel, so one slow
// account cannot hold the admin request open.
func refreshModelRequestConfig(cfg *config.Config, channel string) *config.Config {
	if cfg == nil {
		cfg = &config.Config{}
	} else {
		copyCfg := *cfg
		cfg = &copyCfg
	}

	limit := 0
	if spec, ok := provider.Lookup(channel); ok && spec.Catalog != nil {
		limit = spec.Catalog.RequestTimeoutSeconds
	}
	if limit > 0 && (cfg.RequestTimeout <= 0 || cfg.RequestTimeout > limit) {
		cfg.RequestTimeout = limit
	}

	return cfg
}

// catalogAccounts returns the enabled accounts of a channel that its catalog
// can be read with.
func catalogAccounts(ctx context.Context, s *store.Store, accountType string, catalog *provider.Catalog) ([]*store.Account, error) {
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil || !strings.EqualFold(strings.TrimSpace(acc.AccountType), accountType) {
			continue
		}
		if catalog != nil && catalog.Eligible != nil && !catalog.Eligible(acc) {
			continue
		}
		out = append(out, acc)
	}
	return out, nil
}
