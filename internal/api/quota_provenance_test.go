package api

import (
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/grok"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// buildAccount is a Grok Build (OAuth/Build provider) account with the given plan
// metadata and billing snapshot.
func buildAccount(plan string, billing store.GrokBillingSnapshot) *store.Account {
	return &store.Account{
		ID:           143,
		AccountType:  "grok",
		GrokProvider: grok.ProviderBuild,
		Subscription: plan,
		GrokBilling:  billing,
	}
}

func fieldString(t *testing.T, fields map[string]any, key string) string {
	t.Helper()
	value, _ := fields[key].(string)
	return value
}

func fieldBool(t *testing.T, fields map[string]any, key string) bool {
	t.Helper()
	value, _ := fields[key].(bool)
	return value
}

func fieldFloat(t *testing.T, fields map[string]any, key string) float64 {
	t.Helper()
	value, _ := fields[key].(float64)
	return value
}

// TestBuildQuotaOfficialWindowIsConfirmed pins the first branch of the projection:
// upstream billing is authoritative and must keep its values and provenance.
func TestBuildQuotaOfficialWindowIsConfirmed(t *testing.T) {
	t.Parallel()

	acc := buildAccount("supergrok", store.GrokBillingSnapshot{
		Weekly:   store.GrokQuotaWindow{HasUsage: true, UsagePercent: 42, ResetAt: time.Now().Add(3 * 24 * time.Hour)},
		SyncedAt: time.Now(),
		Source:   "cli_billing",
	})

	fields := buildQuotaResponseFields(acc)
	testutil.Equal(t, fieldString(t, fields, "quota_mode"), "weekly_percent")
	testutil.Equal(t, fieldFloat(t, fields, "quota_used"), 42)
	testutil.Equal(t, fieldString(t, fields, "quota_type"), "paid")
	testutil.Equal(t, fieldString(t, fields, "quota_source"), "upstreamBilling")
	testutil.Equal(t, fieldString(t, fields, "quota_confidence"), "confirmed")
	testutil.False(t, !fieldBool(t, fields, "quota_limit_known"), "quota_limit_known=false for a window upstream actually reported")
}

// TestBuildQuotaInfersFreeAndEstimatesTheWindow pins the reported gap: an OAuth
// account whose upstream publishes neither a plan name nor a numeric window used to
// fall back to "未知", which is indistinguishable from "never synced".
func TestBuildQuotaInfersFreeAndEstimatesTheWindow(t *testing.T) {
	t.Parallel()

	acc := buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now(), Source: "cli_billing"})

	fields := buildQuotaResponseFieldsWithUsage(acc, 12000, true)
	testutil.Equal(t, fieldString(t, fields, "quota_type"), "free")
	testutil.Equal(t, fieldString(t, fields, "quota_source"), grok.FreeProfileSourceBilling)
	testutil.Equal(t, fieldString(t, fields, "quota_confidence"), "estimated")
	testutil.False(t, fieldBool(t, fields, "quota_limit_known"), "quota_limit_known=true for an estimated window: the UI would show it as a balance")
	testutil.False(t, !fieldBool(t, fields, "quota_observed"), "quota_observed=false although this gateway measured the usage")
	testutil.Equal(t, fieldFloat(t, fields, "quota_limit"), float64(grok.EstimatedFreeBuildTokenLimit))
	testutil.Equal(t, fieldFloat(t, fields, "quota_used"), 12000)
	testutil.Equal(t, fieldFloat(t, fields, "quota_remaining"), float64(grok.EstimatedFreeBuildTokenLimit)-12000)
	testutil.Equal(t, fields["quota_window_hours"], 24)
	testutil.Equal(t, fieldString(t, fields, "quota_unit"), "tokens")
	// An unmeasured window must not be presented as measured usage of zero.
	unmeasured := buildQuotaResponseFields(acc)
	testutil.False(t, fieldBool(t, unmeasured, "quota_observed"), "quota_observed=true although nothing was measured")
	testutil.Equal(t, fieldFloat(t, unmeasured, "quota_used"), 0)
}

// TestBuildQuotaFreeFromOfficialPlanName covers the stronger Free signal: the
// upstream identity endpoint named the Free plan.
func TestBuildQuotaFreeFromOfficialPlanName(t *testing.T) {
	t.Parallel()

	acc := buildAccount("free", store.GrokBillingSnapshot{})
	fields := buildQuotaResponseFields(acc)
	testutil.Equal(t, fieldString(t, fields, "quota_type"), "free")
	testutil.Equal(t, fieldString(t, fields, "quota_source"), grok.FreeProfileSourcePlan)
	testutil.Equal(t, fieldString(t, fields, "quota_confidence"), "estimated")
}

// TestBuildQuotaPaidPlanWithoutWindowInventsNothing pins the boundary the estimate
// must never cross: a paid plan whose numeric window upstream does not publish keeps
// an unknown limit and no fabricated allowance.
func TestBuildQuotaPaidPlanWithoutWindowInventsNothing(t *testing.T) {
	t.Parallel()

	for _, plan := range []string{"supergrok", "x_premium", "supergrok_heavy", "supergrok_lite"} {
		acc := buildAccount(plan, store.GrokBillingSnapshot{SyncedAt: time.Now()})
		fields := buildQuotaResponseFields(acc)
		testutil.Equal(t, fieldString(t, fields, "quota_type"), "paid")
		testutil.Equal(t, fieldString(t, fields, "quota_source"), "planMetadata")
		testutil.False(t, fieldBool(t, fields, "quota_limit_known"), "a window upstream never reported cannot be a known limit")
		testutil.Equal(t, fieldFloat(t, fields, "quota_limit"), 0)
		testutil.Falsef(t, fieldBool(t, fields, "quota_supported"), "%s: quota_supported=true for an unknown window", plan)
	}
}

// TestBuildQuotaUnsyncedStaysUnknown pins that "never synced" is not turned into Free:
// an inference needs a signal, not the absence of one.
func TestBuildQuotaUnsyncedStaysUnknown(t *testing.T) {
	t.Parallel()

	acc := buildAccount("", store.GrokBillingSnapshot{})
	fields := buildQuotaResponseFields(acc)
	testutil.Equal(t, fieldString(t, fields, "quota_type"), "unknown")
	testutil.Equal(t, fieldString(t, fields, "quota_confidence"), "")
	testutil.False(t, fieldBool(t, fields, "quota_supported"), "quota_supported=true without any upstream data")
	testutil.NotEqual(t, fieldString(t, fields, "quota_note"), "")
}

// TestFreeProfileInferenceRequiresASignal is the unit-level pin for the rule above.
func TestFreeProfileInferenceRequiresASignal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		acc    *store.Account
		want   bool
		source string
	}{
		{"synced zero billing", buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now()}), true, grok.FreeProfileSourceBilling},
		{"official free plan", buildAccount("free", store.GrokBillingSnapshot{}), true, grok.FreeProfileSourcePlan},
		{"never synced", buildAccount("", store.GrokBillingSnapshot{}), false, ""},
		{"unknown plan", buildAccount("unknown", store.GrokBillingSnapshot{}), false, ""},
		{"paid plan", buildAccount("supergrok", store.GrokBillingSnapshot{SyncedAt: time.Now()}), false, ""},
		{"paid plan with a window", buildAccount("supergrok", store.GrokBillingSnapshot{SyncedAt: time.Now(), Weekly: store.GrokQuotaWindow{HasUsage: true}}), false, ""},
	}
	for _, tc := range cases {
		verdict := grok.InferFreeProfile(tc.acc)
		testutil.Equal(t, verdict.Inferred, tc.want)
		testutil.Equal(t, verdict.Source, tc.source)
	}
}

// TestObservedTokensByAccountUsesOnlyTheFreeWindow pins the measurement behind the
// estimate: usage comes from this gateway's own journal, only inside the window, and a
// failed measurement is reported as "not measured" instead of zero.
func TestObservedTokensByAccountUsesOnlyTheFreeWindow(t *testing.T) {
	s, _ := newTestStore(t, "quota-observed:")
	t.Cleanup(func() { _ = s.Close() })
	a := &API{store: s}

	recent := requestEvent("req-recent", "success", 200)
	recent.AccountID = 143
	recent.InputTokens = 900
	recent.OutputTokens = 1100
	recent.Timestamp = time.Now().Add(-2 * time.Hour)

	older := requestEvent("req-older", "success", 200)
	older.AccountID = 143
	older.InputTokens = 40000
	older.OutputTokens = 40000
	older.Timestamp = time.Now().Add(-30 * time.Hour)

	other := requestEvent("req-other", "success", 200)
	other.AccountID = 144
	other.InputTokens = 5000
	other.Timestamp = time.Now().Add(-time.Hour)

	seedJournal(t, a, []audit.Event{older, recent, other})

	usage, ok := a.observedTokensByAccount(t.Context(), time.Now().Add(-grok.FreeBuildUsageWindow))
	testutil.True(t, ok, "the measurement reported failure on a healthy store")
	testutil.Equal(t, usage[143], 2000)
	testutil.Equal(t, usage[144], 5000)

	acc := buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now()})
	fields := buildQuotaResponseFieldsWithUsage(acc, usage[acc.ID], true)
	testutil.Equal(t, fieldFloat(t, fields, "quota_used"), 2000)
	testutil.False(t, !fieldBool(t, fields, "quota_observed"), "quota_observed=false although the measurement succeeded")
}

// TestBuildQuotaConfirmedFreeWindowReplacesTheEstimate pins the upgrade path: once the
// upstream reports the real actual/limit pair, the estimate is replaced by a confirmed
// balance instead of lingering next to it.
func TestBuildQuotaConfirmedFreeWindowReplacesTheEstimate(t *testing.T) {
	t.Parallel()

	acc := buildAccount("", store.GrokBillingSnapshot{SyncedAt: time.Now()})
	acc.GrokFreeQuota = store.GrokFreeQuotaSnapshot{
		Used: 300000, Limit: 300000, HasLimit: true,
		ConfirmedAt: time.Now(), ResetAt: time.Now().Add(24 * time.Hour),
	}

	fields := buildQuotaResponseFields(acc)
	testutil.Equal(t, fieldString(t, fields, "quota_mode"), "confirmed_free")
	testutil.Equal(t, fieldString(t, fields, "quota_source"), "upstreamExhaustion")
	testutil.Equal(t, fieldString(t, fields, "quota_confidence"), "confirmed")
	testutil.False(t, !fieldBool(t, fields, "quota_limit_known"), "a window the upstream reported has a known limit")
	testutil.Equal(t, fieldFloat(t, fields, "quota_limit"), 300000)
	testutil.Equal(t, fieldFloat(t, fields, "quota_used"), 300000)
	testutil.Equal(t, fieldFloat(t, fields, "quota_remaining"), 0)

	// Once the rolling window has passed, those numbers describe a window that no longer
	// exists: the account falls back to the estimate, but the refusal still proves Free.
	expired := buildAccount("", store.GrokBillingSnapshot{})
	expired.GrokFreeQuota = store.GrokFreeQuotaSnapshot{
		Used: 300000, Limit: 300000, HasLimit: true,
		ConfirmedAt: time.Now().Add(-48 * time.Hour), ResetAt: time.Now().Add(-24 * time.Hour),
	}
	staleFields := buildQuotaResponseFields(expired)
	testutil.Equal(t, fieldString(t, staleFields, "quota_mode"), "estimated_free")
	testutil.Equal(t, fieldString(t, staleFields, "quota_source"), grok.FreeProfileSourceExhaustion)
	testutil.False(t, fieldBool(t, staleFields, "quota_limit_known"), "an expired window cannot still be a known current limit")
	testutil.Equal(t, fieldFloat(t, staleFields, "quota_limit"), float64(grok.EstimatedFreeBuildTokenLimit))
}
