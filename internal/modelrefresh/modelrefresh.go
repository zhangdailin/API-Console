// Package modelrefresh discovers the upstream model catalogs a channel can
// serve and reconciles them into the model directory.
//
// A refresh is an observation, never an opinion: every row this package writes
// comes from a catalog that was read from an upstream service for an account
// that is currently active. A channel with no active account publishes nothing,
// and a partially failed round may add positive observations but never prunes
// last-known-good rows.
package modelrefresh

import (
	"errors"
	"fmt"
	"orchids-api/internal/util"
	"strconv"
	"strings"
)

const (
	defaultModelRefreshConcurrency = 4
	maxModelRefreshConcurrency     = 16
)

type Result struct {
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

// runIndexedModelRefreshWorkers retains the catalog concurrency limits.
func runIndexedModelRefreshWorkers(total, concurrency int, work func(int)) []error {
	if work == nil {
		return nil
	}
	return util.RunIndexed(total, boundedModelRefreshWorkers(total, concurrency), func(index int) error { work(index); return nil })
}
