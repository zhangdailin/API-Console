package grok

import (
	"context"
	"orchids-api/internal/chatwire"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// buildAcc is a Build OAuth account, the only provider the Build Free window (and
// therefore this parser) applies to.
func buildAcc() *store.Account {
	return &store.Account{ID: 143, AccountType: "grok", GrokProvider: ProviderBuild}
}

func TestAccountUsableForModelHonorsBuildFreeReset(t *testing.T) {
	acc := buildAcc()
	acc.GrokFreeQuota.ResetAt = time.Now().Add(time.Hour)
	testutil.False(t, accountUsableForModel(context.Background(), acc), "Build Free account was selected before its confirmed reset")
	acc.GrokFreeQuota.ResetAt = time.Now().Add(-time.Second)
	testutil.False(t, !accountUsableForModel(context.Background(), acc), "Build Free account remained unavailable after its reset")
}

// TestApplyFreeQuotaExhaustionReadsTheRealWindow pins the one place a Free allowance
// stops being an estimate: the refusal the upstream sends after the included free
// usage is spent carries the account's real actual/limit pair.
func TestApplyFreeQuotaExhaustionReadsTheRealWindow(t *testing.T) {
	t.Parallel()

	acc := buildAcc()
	body := []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"You have used all the included free usage for model grok-4.6. Tokens (actual/limit): 500123/500000"}}`)

	testutil.False(t, !ApplyFreeQuotaExhaustion(acc, body), "a Free-usage-exhausted refusal was not recognised")
	testutil.False(t, !acc.GrokFreeQuota.HasLimit, "the actual/limit pair was not read out of the refusal")
	testutil.Equal(t, acc.GrokFreeQuota.Used, 500123)
	testutil.Equal(t, acc.GrokFreeQuota.Limit, 500000)
	testutil.False(t, acc.GrokFreeQuota.ConfirmedAt.IsZero(), "a confirmed window must record when it was confirmed")
	got := time.Until(acc.GrokFreeQuota.ResetAt)
	testutil.Falsef(t, got <= 0 || got > FreeBuildUsageWindow+time.Minute, "reset in %v, want within the rolling %v window", got, FreeBuildUsageWindow)
	// The confirmed window is the strongest Free signal there is.
	verdict := InferFreeProfile(acc)
	testutil.Falsef(t, !verdict.Inferred || verdict.Source != FreeProfileSourceExhaustion, "verdict = %+v, want an inference sourced from %q", verdict, FreeProfileSourceExhaustion)
}

// TestApplyFreeQuotaExhaustionWithoutNumbers pins the degraded case: the refusal is
// still proof of a Free account even when the pair cannot be parsed, and it must not
// be recorded as a limit of zero.
func TestApplyFreeQuotaExhaustionWithoutNumbers(t *testing.T) {
	t.Parallel()

	acc := buildAcc()
	body := []byte(`{"error":{"code":"subscription:free-usage-exhausted"}}`)
	testutil.False(t, !ApplyFreeQuotaExhaustion(acc, body), "a Free refusal without numbers was not recognised")
	testutil.False(t, acc.GrokFreeQuota.HasLimit, "HasLimit=true without a readable pair")
	testutil.False(t, acc.GrokFreeQuota.ConfirmedAt.IsZero(), "the confirmation timestamp is still useful knowledge")
	verdict := InferFreeProfile(acc)
	testutil.Falsef(t, !verdict.Inferred || verdict.Source != FreeProfileSourceExhaustion, "verdict = %+v, want a Free inference from the refusal", verdict)
}

// TestApplyFreeQuotaExhaustionKeepsAPreviouslyReportedLimit pins that a later refusal
// which cannot be parsed does not downgrade the window back to "unknown number": the
// real actual/limit pair stays until the upstream reports a different one.
func TestApplyFreeQuotaExhaustionKeepsAPreviouslyReportedLimit(t *testing.T) {
	t.Parallel()

	acc := buildAcc()
	testutil.False(t, !ApplyFreeQuotaExhaustion(acc, []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"Tokens (actual/limit): 500123/500000"}}`)), "the first refusal was not recognised")
	confirmedAt := acc.GrokFreeQuota.ConfirmedAt

	testutil.False(t, !ApplyFreeQuotaExhaustion(acc, []byte(`{"error":{"code":"subscription:free-usage-exhausted"}}`)), "the second refusal was not recognised")
	testutil.Falsef(t, !acc.GrokFreeQuota.HasLimit || acc.GrokFreeQuota.Limit != 500000 || acc.GrokFreeQuota.Used != 500123, "the previously confirmed pair was lost: %+v", acc.GrokFreeQuota)
	testutil.False(t, !acc.GrokFreeQuota.ConfirmedAt.After(confirmedAt), "the confirmation timestamp was not refreshed")

	// A NEWER refusal with a different pair is the truth for the current window.
	testutil.False(t, !ApplyFreeQuotaExhaustion(acc, []byte(`{"error":{"code":"subscription:free-usage-exhausted","message":"Tokens (actual/limit): 12/900000"}}`)), "the third refusal was not recognised")
	testutil.Equal(t, acc.GrokFreeQuota.Limit, 900000)
	testutil.Equal(t, acc.GrokFreeQuota.Used, 12)
}

func TestApplyFreeQuotaExhaustionIgnoresEverythingElse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		acc  *store.Account
		body string
	}{
		{"ordinary 429", buildAcc(), `{"error":{"code":"rate_limit_exceeded"}}`},
		{"paid spending limit", buildAcc(), `{"error":{"code":"personal-team-blocked:spending-limit"}}`},
		{"empty body", buildAcc(), ``},
	}
	for _, tc := range cases {
		testutil.Falsef(t, ApplyFreeQuotaExhaustion(tc.acc, []byte(tc.body)), "%s: the response was recorded as a Free refusal", tc.name)
		testutil.Falsef(t, !tc.acc.GrokFreeQuota.ConfirmedAt.IsZero(), "%s: the account was modified by an unrecognised response", tc.name)
	}
}

func TestInferFreeProfileDoesNotClaimPaidAccountsAsFree(t *testing.T) {
	t.Parallel()

	paid := []string{"supergrok", "XPremium", "x_premium_plus", "heavy", "lite", "Pro", "team", "enterprise"}
	for _, plan := range paid {
		acc := &store.Account{AccountType: "grok", CredentialType: "oauth", Subscription: plan}
		verdict := InferFreeProfile(acc)
		testutil.CheckFalsef(t, verdict.Inferred, "plan %q was inferred Free", plan)
	}

	// A billing profile with a real window is a paid/entitled account even when
	// the plan string is absent.
	entitled := &store.Account{AccountType: "grok", CredentialType: "oauth", Subscription: "unknown"}
	entitled.GrokBilling.SyncedAt = time.Now()
	entitled.GrokBilling.Weekly.HasUsage = true
	verdict := InferFreeProfile(entitled)
	testutil.CheckFalse(t, verdict.Inferred, "an account with a reported weekly window was inferred Free")

	// No evidence at all stays unknown rather than being guessed at.
	unsynced := &store.Account{AccountType: "grok", CredentialType: "oauth"}
	verdict = InferFreeProfile(unsynced)
	testutil.CheckFalse(t, verdict.Inferred, "an unsynced account was inferred Free")
}

func TestInferSubscriptionFromRateLimitInfoRequiresKnownShapes(t *testing.T) {
	cases := map[int64]string{
		7: "basic", 20: "basic", 8: "basic", 30: "basic",
		50: "super", 140: "super",
		150: "heavy",
		25:  "lite", 70: "lite", 12: "lite",
		1000: "", 151: "", 149: "", 3: "",
	}
	for limit, want := range cases {
		got := inferSubscriptionFromRateLimitInfo(&chatwire.RateLimitInfo{Limit: limit, HasLimit: true})
		testutil.Equal(t, got, want)
	}
}

// The unused heavy mode still identifies the top tier when the upstream sends it.
