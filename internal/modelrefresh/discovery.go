package modelrefresh

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/workbuddy"
)

func discoverModelsForChannelReport(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (accountModelDiscoveryReport, error) {
	switch strings.ToLower(channel) {
	case "workbuddy", "qoder", "cline":
		return discoverAccountCatalogModels(ctx, cfg, s, channel, concurrency)
	case "grok":
		return discoverGrokModelsReport(ctx, cfg, s, concurrency)
	default:
		return accountModelDiscoveryReport{}, fmt.Errorf("unsupported channel: %s", channel)
	}
}

// qoderCatalogToDiscovered maps the account catalog onto the channel's public
// model records.
//
// The public identifier is the display name, not the internal gateway key: the
// channel resolves either form, and a client that saw "Qwen3.7-Max" in
// /v1/models must be able to ask for it by that name. The internal key stays in
// the account snapshot, which is what the client resolves against.
func qoderCatalogToDiscovered(catalog *qoder.Catalog) []discoveredModel {
	entries := catalog.Entries()
	out := make([]discoveredModel, 0, len(entries))
	for i, entry := range entries {
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			continue
		}
		id := strings.TrimSpace(entry.Name)
		if id == "" {
			id = key
		}
		// Model lookup is lowercased before it reaches the store index, so the
		// public identifier is stored in lowercase. The upstream key and the
		// display name are both still accepted at request time, because the
		// catalog resolves case-insensitively.
		id = strings.ToLower(id)
		candidate := discoveredModel{ID: id, Name: id, SortOrder: i, Verified: true}
		if entry.PriceFactor != nil && *entry.PriceFactor == 0 {
			candidate.BillingTier = "free"
			candidate.BillingSource = "qoder_price_factor"
		} else if entry.PriceFactor != nil {
			candidate.BillingTier = "metered"
			candidate.BillingSource = "qoder_price_factor"
		}
		out = append(out, candidate)
	}
	return out
}

// PersistAccountCatalogSnapshot keeps the last usable account snapshot when an
// upstream catalog is empty. Callers supply their own failure context so manual
// discovery and background refresh retain their distinct log messages.
func PersistAccountCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, channel string, ids []string, updateFailureMessage string) {
	if acc == nil || len(ids) == 0 {
		return
	}
	now := time.Now()
	switch channel {
	case "qoder":
		acc.QoderModelIDs, acc.QoderModelsSyncedAt = ids, now
	case "workbuddy":
		acc.WorkBuddyModelIDs, acc.WorkBuddyModelsSyncedAt = ids, now
	case "cline":
		acc.ClineModelIDs, acc.ClineModelsSyncedAt = ids, now
	default:
		return
	}
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn(updateFailureMessage, "account_id", acc.ID, "error", err)
	}
}

// persistQoderCatalogSnapshot records the account-scoped upstream catalog so
// model selection resolves against the same list the channel publishes.
func persistQoderCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, catalog *qoder.Catalog) {
	if acc == nil || acc.ID == 0 {
		return
	}
	PersistAccountCatalogSnapshot(ctx, s, acc, "qoder", qoder.CatalogSnapshot(catalog), "failed to persist qoder model snapshot")
}

func discoverAccountCatalogModels(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (accountModelDiscoveryReport, error) {
	channel = normalizeAdminModelChannel(channel)
	accountType := strings.ToLower(channel)
	source := map[string]string{
		"workbuddy": "workbuddy_cli_models",
		"qoder":     "qoder_upstream_models",
		"cline":     "cline_recommended_models",
	}[accountType]
	if source == "" {
		return accountModelDiscoveryReport{}, fmt.Errorf("unsupported account catalog channel: %s", channel)
	}
	accounts, err := enabledAccountsByType(ctx, s, accountType)
	if err != nil {
		return accountModelDiscoveryReport{}, fmt.Errorf("%s model discovery failed: %w", accountType, err)
	}
	if len(accounts) == 0 {
		return accountModelDiscoveryReport{}, &noActiveAccountsError{Channel: channel}
	}

	report := accountModelDiscoveryReport{Source: source, Attempts: make([]accountModelDiscoveryAttempt, len(accounts))}
	runIndexedModelRefreshWorkers(len(accounts), concurrency, func(index int) {
		acc := accounts[index]
		attempt := accountModelDiscoveryAttempt{AccountID: acc.ID}
		switch accountType {
		case "workbuddy":
			client := workbuddy.NewFromAccount(acc, refreshModelRequestConfig(cfg, accountType))
			client.SetAccountStore(s)
			models, fetchErr := client.FetchModels(ctx)
			client.Close()
			attempt.Err = fetchErr
			attempt.Candidates = workBuddyCatalogToDiscovered(models)
			if fetchErr == nil && len(attempt.Candidates) > 0 {
				persistWorkBuddyCatalogSnapshot(ctx, s, acc, models)
			}
		case "qoder":
			client := qoder.NewFromAccount(acc, refreshModelRequestConfig(cfg, accountType))
			client.SetAccountStore(s)
			catalog, fetchErr := client.FetchUpstreamModels(ctx)
			client.Close()
			attempt.Err = fetchErr
			attempt.Candidates = qoderCatalogToDiscovered(catalog)
			if fetchErr == nil && len(attempt.Candidates) > 0 {
				persistQoderCatalogSnapshot(ctx, s, acc, catalog)
			}
		case "cline":
			client := cline.NewFromAccount(acc, refreshModelRequestConfig(cfg, accountType))
			client.SetAccountStore(s)
			models, fetchErr := client.FetchUpstreamModels(ctx)
			client.Close()
			attempt.Err = fetchErr
			attempt.Candidates = clineCatalogToDiscovered(models)
			if fetchErr == nil && len(attempt.Candidates) > 0 {
				persistClineCatalogSnapshot(ctx, s, acc, models)
			}
		}
		if attempt.Err == nil && len(attempt.Candidates) == 0 {
			attempt.Err = fmt.Errorf("%s account #%d returned an empty upstream catalog", accountType, acc.ID)
		}
		if attempt.Err != nil {
			attempt.Candidates = accountCatalogSnapshotToDiscovered(accountType, acc)
			attempt.UsedLastKnownGood = len(attempt.Candidates) > 0
		}
		report.Attempts[index] = attempt
	})

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

type storedAccountCatalogRow struct {
	ID          string   `json:"id"`
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Provider    string   `json:"provider"`
	PriceFactor *float64 `json:"price_factor"`
}

func accountCatalogSnapshotToDiscovered(channel string, acc *store.Account) []discoveredModel {
	if acc == nil {
		return nil
	}
	var rows []string
	switch channel {
	case "workbuddy":
		rows = acc.WorkBuddyModelIDs
	case "qoder":
		rows = acc.QoderModelIDs
	case "cline":
		rows = acc.ClineModelIDs
	}
	out := make([]discoveredModel, 0, len(rows))
	for i, raw := range rows {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		row := storedAccountCatalogRow{ID: trimmed}
		if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &row) != nil {
			continue
		}
		id := strings.TrimSpace(row.ID)
		if channel == "qoder" {
			id = util.FirstNonEmpty(strings.TrimSpace(row.Name), strings.TrimSpace(row.DisplayName), strings.TrimSpace(row.Key), id)
			id = strings.ToLower(id)
		}
		if id == "" {
			continue
		}
		candidate := discoveredModel{
			ID: id, Name: util.FirstNonEmpty(strings.TrimSpace(row.Name), strings.TrimSpace(row.DisplayName), id),
			SortOrder: i, Verified: true,
		}
		if channel == "cline" {
			candidate.Provider = strings.TrimSpace(row.Provider)
			candidate.UpstreamModel = id
		}
		if channel == "qoder" && row.PriceFactor != nil {
			candidate.BillingTier = "metered"
			candidate.BillingSource = "qoder_price_factor"
			if *row.PriceFactor == 0 {
				candidate.BillingTier = "free"
			}
		}
		out = append(out, candidate)
	}
	return out
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

// clineCatalogToDiscovered maps the observed feed onto the channel's public
// model records.
//
// The public identifier is the upstream id: a client that saw it in /v1/models
// must be able to ask for it by that name. The display name is the one the feed
// published next to it, so the model list reads like the upstream's own catalog
// instead of repeating the identifier in both columns.
//
// Provider is the vendor half the feed names before the first "/". It is what
// the public list reports as owned_by, and it is the only distinction between
// two free models of the same channel.
func clineCatalogToDiscovered(models []cline.Model) []discoveredModel {
	out := make([]discoveredModel, 0, len(models))
	for i, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		out = append(out, discoveredModel{
			ID:            id,
			Name:          util.FirstNonEmpty(strings.TrimSpace(model.Name), id),
			SortOrder:     i,
			Verified:      true,
			Provider:      strings.TrimSpace(model.Provider),
			UpstreamModel: id,
		})
	}
	return out
}

// persistClineCatalogSnapshot records the account-scoped upstream catalog so
// model selection resolves against the same list the channel publishes.
func persistClineCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, models []cline.Model) {
	if acc == nil || acc.ID == 0 {
		return
	}
	PersistAccountCatalogSnapshot(ctx, s, acc, "cline", cline.CatalogSnapshot(models), "failed to persist cline model snapshot")
}

func workBuddyCatalogToDiscovered(models []workbuddy.WorkBuddyModel) []discoveredModel {
	out := make([]discoveredModel, 0, len(models))
	for i, model := range models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		name := strings.TrimSpace(model.Name)
		if name == "" {
			name = id
		}
		out = append(out, discoveredModel{ID: id, Name: name, SortOrder: i, Verified: true})
	}
	return out
}

// persistWorkBuddyCatalogSnapshot records the account-scoped whitelist so model
// selection can be checked against what this account may actually run. Each row
// keeps the window the catalog declared, because the public model list has to
// report a real number for a client that budgets its context.
func persistWorkBuddyCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, models []workbuddy.WorkBuddyModel) {
	if acc == nil || acc.ID == 0 {
		return
	}
	PersistAccountCatalogSnapshot(ctx, s, acc, "workbuddy", workbuddy.CatalogSnapshot(models), "failed to persist workbuddy model snapshot")
}

func refreshModelRequestConfig(cfg *config.Config, channel string) *config.Config {
	if cfg == nil {
		cfg = &config.Config{}
	} else {
		copyCfg := *cfg
		cfg = &copyCfg
	}

	switch strings.ToLower(strings.TrimSpace(channel)) {
	case "workbuddy", "qoder", "cline":
		if cfg.RequestTimeout <= 0 || cfg.RequestTimeout > 15 {
			cfg.RequestTimeout = 15
		}
	}

	return cfg
}

func enabledAccountsByType(ctx context.Context, s *store.Store, accountType string) ([]*store.Account, error) {
	accounts, err := s.GetEnabledAccounts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(acc.AccountType), accountType) {
			out = append(out, acc)
		}
	}
	return out, nil
}
