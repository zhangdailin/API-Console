package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/channel"
	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/grok"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"orchids-api/internal/workbuddy"
)

const (
	defaultModelRefreshConcurrency = 4
	maxModelRefreshConcurrency     = 16
)

// fetchGrokBuildModelsForRefresh reads the official Build CLI catalog.  It is
// deliberately kept as an injectable control-plane operation: model refresh
// must never send a completion simply to discover an account's capabilities.
var fetchGrokBuildModelsForRefresh = func(ctx context.Context, cfg *config.Config, s *store.Store, acc *store.Account) ([]modelcatalog.Profile, error) {
	client := grok.NewCLIClient(cfg)
	client.SetAccountStore(s)
	return client.FetchModelCatalog(ctx, acc)
}

type modelRefreshRequest struct {
	Channel     string `json:"channel"`
	Concurrency int    `json:"concurrency,omitempty"`
}

type modelRefreshResult struct {
	Channel           string   `json:"channel"`
	Source            string   `json:"source"`
	Outcome           string   `json:"outcome,omitempty"`
	Concurrency       int      `json:"concurrency"`
	AccountsTotal     int      `json:"accounts_total,omitempty"`
	AccountsSuccess   int      `json:"accounts_succeeded,omitempty"`
	AccountsFailed    int      `json:"accounts_failed,omitempty"`
	Partial           bool     `json:"partial,omitempty"`
	KeptLastKnownGood bool     `json:"kept_last_known_good,omitempty"`
	Discovered        int      `json:"discovered"`
	Verified          int      `json:"verified"`
	Added             int      `json:"added"`
	Updated           int      `json:"updated"`
	Deleted           int      `json:"deleted"`
	Offline           int      `json:"offline"`
	Skipped           bool     `json:"skipped,omitempty"`
	DefaultModelID    string   `json:"default_model_id,omitempty"`
	AddedModelIDs     []string `json:"added_model_ids,omitempty"`
	DeletedModelIDs   []string `json:"deleted_model_ids,omitempty"`
	OfflineModelIDs   []string `json:"offline_model_ids,omitempty"`
}

// accountModelDiscoveryAttempt is the account-level evidence retained until
// aggregation completes. It records successful observations and failures without
// forcing the public refresh API to expose account identities.
type accountModelDiscoveryAttempt struct {
	AccountID         int64
	Candidates        []discoveredModel
	Err               error
	UsedLastKnownGood bool
}

// accountModelDiscoveryReport carries the union plus enough attempt metadata to
// decide whether it is authoritative. A partial union may add/update observed
// rows, but must never prune rows absent from a failed account's view.
type accountModelDiscoveryReport struct {
	Candidates []discoveredModel
	Source     string
	Attempts   []accountModelDiscoveryAttempt
}

func (r accountModelDiscoveryReport) counts() (succeeded, failed int) {
	for _, attempt := range r.Attempts {
		if attempt.Err != nil || len(attempt.Candidates) == 0 {
			failed++
			continue
		}
		succeeded++
	}
	return succeeded, failed
}

type discoveredModel struct {
	ID        string
	Name      string
	SortOrder int
	// Verified marks a candidate this refresh actually observed as usable
	// upstream (a catalog read that succeeded, or a probe that accepted it).
	// It is set per candidate rather than inferred from the candidate count, so
	// "discovered" and "verified" stay distinguishable in the admin report.
	Verified bool
	// Provider and UpstreamModel are the route metadata a catalog read observed
	// alongside the identifier. They stay empty for a channel whose feed names
	// neither, and a row keeps whatever it already had in that case.
	Provider      string
	UpstreamModel string
	// BillingTier is dynamic upstream metadata, not a name-based guess. Only
	// "free" grants routing to an account whose metered allowance is exhausted.
	BillingTier   string
	BillingSource string
}

// noActiveAccountsError reports that a channel has no account eligible for an
// upstream catalog read. Model management publishes nothing in that state:
// every published row has to be an observation of an upstream catalog, never a
// locally compiled-in default.
type noActiveAccountsError struct {
	Channel string
}

func (e *noActiveAccountsError) Error() string {
	return fmt.Sprintf("%s has no active account; model refresh only publishes upstream catalogs", e.Channel)
}

func isNoActiveAccounts(err error) bool {
	var target *noActiveAccountsError
	return errors.As(err, &target)
}

// upstreamCatalogSources are the only refresh sources allowed to publish model
// rows. Each one names a catalog that was read from an upstream service for an
// account that is currently active. A cached list, an unverified public list or
// a compiled-in catalog is not an observation and must not reach the store.
//
// The match is exact rather than by prefix: "grok_build_models" is an
// observation while "grok_build_models_unavailable_cached" is not, and a prefix
// test would accept the latter.
var upstreamCatalogSources = map[string]struct{}{
	"grok_build_models":        {},
	"workbuddy_cli_models":     {},
	"qoder_upstream_models":    {},
	"cline_recommended_models": {},
}

// A catalog source that names the upstream read that produced it is an
// observation and may publish model rows. The match is exact rather than by
// prefix: "grok_build_models" is an observation while
// "grok_build_models_unavailable_cached" is not, and a prefix test would accept
// the latter.
func isUpstreamCatalogSource(source string) bool {
	_, ok := upstreamCatalogSources[strings.TrimSpace(source)]
	return ok
}

type modelRefreshFunc func(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error)

var runModelRefresh modelRefreshFunc = syncModelsForChannelConcurrent

type modelRefreshCoordinator struct {
	mu      sync.Mutex
	running map[string]struct{}
}

func newModelRefreshCoordinator() *modelRefreshCoordinator {
	return &modelRefreshCoordinator{running: make(map[string]struct{})}
}

func (c *modelRefreshCoordinator) tryAcquire(channel string) (func(), bool) {
	if c == nil {
		return func() {}, true
	}
	key := strings.ToLower(strings.TrimSpace(channel))
	if key == "" {
		key = "*"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.running[key]; exists {
		return nil, false
	}
	c.running[key] = struct{}{}
	return func() {
		c.mu.Lock()
		delete(c.running, key)
		c.mu.Unlock()
	}, true
}

func acquireDistributedModelRefresh(ctx context.Context, s *store.Store, channel string) (func(), bool) {
	if s == nil || s.RedisClient() == nil {
		return func() {}, true
	}
	key := s.RedisPrefix() + "lease:model_refresh:" + strings.ToLower(strings.TrimSpace(channel))
	token := fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().UTC().Unix())
	acquired, err := s.RedisClient().SetNX(ctx, key, token, 30*time.Minute).Result()
	if err != nil || !acquired {
		return nil, false
	}
	return func() {
		// Delete only our own lease; an expired lease may already belong to a newer run.
		const releaseScript = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) end return 0`
		_, _ = s.RedisClient().Eval(context.Background(), releaseScript, []string{key}, token).Result()
	}, true
}

func makeCoordinatedModelRefreshHandler(configSnapshot func() *config.Config, s *store.Store, coordinator *modelRefreshCoordinator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		channel := strings.TrimSpace(r.URL.Query().Get("channel"))
		concurrency := defaultModelRefreshConcurrency
		if parsed, ok := parseModelRefreshConcurrency(r.URL.Query().Get("concurrency")); ok {
			concurrency = parsed
		}
		if r.Body != nil {
			defer r.Body.Close()
			var req modelRefreshRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err == nil && strings.TrimSpace(req.Channel) != "" {
				channel = strings.TrimSpace(req.Channel)
			}
			if req.Concurrency != 0 {
				concurrency = normalizeModelRefreshConcurrency(req.Concurrency)
			}
		}

		release, acquired := coordinator.tryAcquire(channel)
		if !acquired {
			http.Error(w, "model refresh already running for this channel", http.StatusConflict)
			return
		}
		defer release()
		distributedRelease, distributedAcquired := acquireDistributedModelRefresh(r.Context(), s, channel)
		if !distributedAcquired {
			http.Error(w, "model refresh already running for this channel", http.StatusConflict)
			return
		}
		defer distributedRelease()
		cfg := configSnapshot()
		result, err := runModelRefresh(r.Context(), cfg, s, channel, concurrency)
		if err != nil {
			// No active account is a legitimate state, not a failure: nothing was
			// fetched, so nothing is published. It is reported as a skipped
			// refresh so the admin page can distinguish it from an upstream
			// outage and from a successful empty refresh.
			if isNoActiveAccounts(err) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(&modelRefreshResult{
					Channel:     normalizeAdminModelChannel(channel),
					Source:      "no_active_account",
					Concurrency: concurrency,
					Skipped:     true,
				})
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if result != nil && result.Concurrency == 0 {
			result.Concurrency = concurrency
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(result); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func syncModelsForChannelConcurrent(ctx context.Context, cfg *config.Config, s *store.Store, channel string, concurrency int) (*modelRefreshResult, error) {
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

func parseModelRefreshConcurrency(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return defaultModelRefreshConcurrency, true
	}
	return normalizeModelRefreshConcurrency(value), true
}

func normalizeModelRefreshConcurrency(concurrency int) int {
	if concurrency <= 0 {
		return defaultModelRefreshConcurrency
	}
	if concurrency > maxModelRefreshConcurrency {
		return maxModelRefreshConcurrency
	}
	return concurrency
}

func boundedModelRefreshWorkers(total int, concurrency int) int {
	if total <= 0 {
		return 0
	}
	workers := normalizeModelRefreshConcurrency(concurrency)
	if workers > total {
		workers = total
	}
	return workers
}

// runIndexedModelRefreshWorkers applies work to every index using at most the
// configured number of workers. Each index is handed out once, so callers can
// safely write results[index] without a mutex and retain input ordering.
func runIndexedModelRefreshWorkers(total, concurrency int, work func(index int)) {
	workerCount := boundedModelRefreshWorkers(total, concurrency)
	if workerCount == 0 || work == nil {
		return
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for index := range jobs {
				work(index)
			}
		}()
	}
	for index := range total {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
}

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

// persistQoderCatalogSnapshot records the account-scoped upstream catalog so
// model selection resolves against the same list the channel publishes.
func persistQoderCatalogSnapshot(ctx context.Context, s *store.Store, acc *store.Account, catalog *qoder.Catalog) {
	if acc == nil || acc.ID == 0 {
		return
	}
	ids := qoder.CatalogSnapshot(catalog)
	if len(ids) == 0 {
		return
	}
	acc.QoderModelIDs = ids
	acc.QoderModelsSyncedAt = time.Now()
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("failed to persist qoder model snapshot", "account_id", acc.ID, "error", err)
	}
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
			models, fetchErr := client.FetchModels(ctx)
			client.Close()
			attempt.Err = fetchErr
			attempt.Candidates = workBuddyCatalogToDiscovered(models)
			if fetchErr == nil && len(attempt.Candidates) > 0 {
				persistWorkBuddyCatalogSnapshot(ctx, s, acc, models)
			}
		case "qoder":
			client := qoder.NewFromAccount(acc, refreshModelRequestConfig(cfg, accountType))
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
	ids := cline.CatalogSnapshot(models)
	if len(ids) == 0 {
		return
	}
	acc.ClineModelIDs = ids
	acc.ClineModelsSyncedAt = time.Now()
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("failed to persist cline model snapshot", "account_id", acc.ID, "error", err)
	}
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
	ids := workbuddy.CatalogSnapshot(models)
	if len(ids) == 0 {
		return
	}
	acc.WorkBuddyModelIDs = ids
	acc.WorkBuddyModelsSyncedAt = time.Now()
	if err := s.UpdateAccount(ctx, acc); err != nil {
		slog.Warn("failed to persist workbuddy model snapshot", "account_id", acc.ID, "error", err)
	}
}

type grokBuildModelDiscovery struct {
	index   int
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
	runIndexedModelRefreshWorkers(len(accounts), concurrency, func(index int) {
		acc := accounts[index]
		catalog, fetchErr := fetchGrokBuildModelsForRefresh(ctx, cfg, s, acc)
		ordered[index] = grokBuildModelDiscovery{account: acc, catalog: catalog, err: fetchErr}
	})

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
			} else if spec.Upstream != grok.UpstreamCLI {
				continue
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

func applyModelRefreshWithPrune(ctx context.Context, s *store.Store, channel string, source string, candidates []discoveredModel, allowPrune bool) (*modelRefreshResult, error) {
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

	result := &modelRefreshResult{
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

	defaultModelID := chooseRefreshedDefaultModel(channel, existingByID, candidates)
	result.DefaultModelID = defaultModelID

	records := make([]*store.Model, 0, len(candidates))
	for _, model := range candidates {
		record := &store.Model{
			Channel: channel, ModelID: model.ID, Name: util.FirstNonEmpty(model.Name, model.ID),
			Status: store.ModelStatusAvailable, Verified: model.Verified, IsDefault: model.ID == defaultModelID,
			SortOrder: model.SortOrder, Provider: model.Provider, UpstreamModel: model.UpstreamModel,
			BillingTier: strings.ToLower(strings.TrimSpace(model.BillingTier)), BillingSource: strings.ToLower(strings.TrimSpace(model.BillingSource)),
			Origin: "discovery",
		}
		if strings.EqualFold(strings.TrimSpace(channel), "grok") {
			store.ApplyGrokRouteDefaults(record)
			record.Provider, record.UpstreamModel = grok.ProviderBuild, model.ID
			if strings.Contains(strings.ToLower(model.ID), "video") {
				record.Capabilities = []string{store.CapabilityVideo}
			} else {
				record.Capabilities = []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses}
			}
		}
		records = append(records, record)
	}
	reconcileOptions := store.ModelReconcileOptions{
		Prune: allowPrune && shouldDeleteMissingModelsOnRefresh(channel, source),
	}
	if strings.EqualFold(strings.TrimSpace(channel), "grok") && source == "grok_build_models" && allowPrune {
		reconcileOptions.Prune = true
		reconcileOptions.ProviderScope = grok.ProviderBuild
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
func shouldDeleteMissingModelsOnRefresh(channel, source string) bool {
	source = strings.TrimSpace(source)
	if source == "grok_build_models" || source == "workbuddy_cli_models" {
		// Grok is a provider subset; catalog reads may be inconclusive because of
		// quota/transport; WorkBuddy can return a degraded whitelist fallback.
		// Absence from any of these is not authoritative deletion evidence.
		return false
	}
	return isUpstreamCatalogSource(source)
}

func chooseRefreshedDefaultModel(channel string, existing map[string]*store.Model, ordered []discoveredModel) string {
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
