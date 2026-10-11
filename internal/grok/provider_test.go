package grok

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestBuildCapabilitySnapshotAndRateLimitsDoNotBecomeBilling(t *testing.T) {
	acc := &store.Account{
		AccountType:    "grok",
		CredentialType: "oauth",
		UsageCurrent:   8300,
		UsageLimit:     8300,
	}
	headers := make(http.Header)
	headers.Set("x-ratelimit-limit-requests", "20")
	headers.Set("x-ratelimit-remaining-requests", "19")
	headers.Set("x-ratelimit-limit-tokens", "8300")
	headers.Set("x-ratelimit-remaining-tokens", "8192")
	testutil.False(t, !ApplyBuildRateLimits(acc, headers), "ApplyBuildRateLimits() = false")
	testutil.Falsef(t, acc.GrokRateLimits.Tokens.Limit != 8300 || acc.GrokBilling.Weekly.HasUsage, "rate limits/billing mixed: %+v %+v", acc.GrokRateLimits, acc.GrokBilling)
	testutil.False(t, !ApplyCLIBillingInfo(acc, &CLIBillingInfo{UsagePercent: 12, HasUsagePercent: true, PeriodEnd: time.Now().Add(time.Hour)}), "ApplyCLIBillingInfo() = false")
	testutil.Equal(t, acc.UsageCurrent, 0)
	testutil.Equal(t, acc.UsageLimit, 0)
	testutil.Falsef(t, !acc.GrokBilling.Weekly.HasUsage || acc.GrokBilling.Weekly.UsagePercent != 12 || acc.GrokRateLimits.Tokens.Limit != 8300, "billing/rate-limit separation lost: %+v %+v", acc.GrokBilling, acc.GrokRateLimits)
}

func TestAccountSupportsModelUsesObservedBuildCatalog(t *testing.T) {
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth"}
	testutil.False(t, AccountSupportsModel(acc, "grok-4.6"), "unsynced account must not grant model access")
	ApplyCLIModelCatalog(acc, []modelcatalog.Profile{{ModelID: "grok-4.5"}}, time.Now())
	testutil.Falsef(t, AccountSupportsModel(acc, "grok-4.6") || !AccountSupportsModel(acc, "grok-4.5"), "observed catalog not enforced: %#v", acc.GrokModels)
}

// TestApplyCLIModelCatalogContainsOnlyObservedModels checks observed
// rows only, independently of subscription and credential type.
func TestApplyCLIModelCatalogContainsOnlyObservedModels(t *testing.T) {
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", GrokProvider: ProviderBuild, Subscription: "super"}
	ApplyCLIModelCatalog(acc, []modelcatalog.Profile{{ModelID: "grok-4.6"}, {ModelID: "grok-imagine-video-1.5"}, {ModelID: "grok-4.6"}}, time.Now())

	want := []string{"grok-4.6", "grok-imagine-video-1.5"}
	testutil.Equal(t, len(acc.GrokModels), len(want))
	for i, model := range want {
		testutil.Falsef(t, !strings.EqualFold(acc.GrokModels[i], model), "catalog = %#v, want %#v", acc.GrokModels, want)
	}
	for _, absent := range []string{"grok-4.5", "grok-composer-2.5-fast"} {
		testutil.False(t, AccountSupportsModel(acc, absent), "unobserved model granted")
	}
	testutil.False(t, acc.GrokModelsSyncedAt.IsZero(), "the snapshot was not dated")
}

func TestCatalogRefreshRemovesDerivedIDsAndPreservesLastGoodOnEmpty(t *testing.T) {
	now := time.Now().UTC()
	acc := &store.Account{AccountType: "grok", CredentialType: "oauth", GrokModels: []string{"grok-4.7", "grok-composer-2.5-fast"}}
	ApplyCLIModelCatalog(acc, []modelcatalog.Profile{{ModelID: "future-private-model", ReasoningEfforts: []string{"balanced"}, DefaultReasoningEffort: "balanced"}}, now)
	testutil.Equal(t, strings.Join(acc.GrokModels, ","), "future-private-model")
	spec, effort, ok := ResolveModelAlias("future-private-model-balanced", acc.GrokModelCatalog)
	testutil.True(t, ok, "new upstream effort must not require a code change")
	testutil.Equal(t, spec.UpstreamModel, "future-private-model")
	testutil.Equal(t, effort, "balanced")
	testutil.False(t, ApplyCLIModelCatalog(acc, nil, now.Add(time.Minute)), "empty catalog changed last-good state")
	testutil.Equal(t, strings.Join(acc.GrokModels, ","), "future-private-model")
	testutil.Equal(t, acc.GrokModelsSyncedAt, now)
}

func TestObservedExactEffortSuffixWinsOverAlias(t *testing.T) {
	catalog := []modelcatalog.Profile{{ModelID: "future", ReasoningEfforts: []string{"high"}}, {ModelID: "future-high"}}
	spec, effort, ok := ResolveModelAlias("future-high", catalog)
	testutil.True(t, ok, "exact observed ID not resolved")
	testutil.Equal(t, spec.UpstreamModel, "future-high")
	testutil.Equal(t, effort, "")
}

func TestCatalogExplicitNoReasoningIsAuthoritative(t *testing.T) {
	no := false
	profile := (buildModelCatalogEntry{ID: "grok-4.7", SupportsReasoningEffort: &no, ReasoningEfforts: []json.RawMessage{json.RawMessage(`"high"`)}}).profile()
	testutil.False(t, profile.SupportsReasoningEffort, "explicit false ignored")
	testutil.Equal(t, len(profile.ReasoningEfforts), 0)
}
