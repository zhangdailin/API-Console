package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func timeDate(year int, month time.Month, day, hour, min, sec int) time.Time {
	return time.Date(year, month, day, hour, min, sec, 0, time.UTC)
}

// TestFetchQuotaReadsTheWindowAndPlan proves the channel can describe an
// account's allowance. These two OpenAPI reads are what turn "the account
// failed" into "the account has no credits left", which is the difference
// between chasing a credential bug and acting on a billing state.
func TestFetchQuotaReadsTheWindowAndPlan(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.CheckEqual(t, r.Header.Get("Authorization"), "Bearer access-1")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userId":"uid-1","userType":"personal_standard","usageType":"credits","isQuotaExceeded":true,"expiresAt":253402214400000,"upgradeUrl":"https://qoder.com/pricing?client=qoder","userQuota":{"total":0,"used":0,"remaining":0,"percentage":0,"unit":"credits"}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"user_type":"personal_standard","plan_tier_name":"Free","is_personal_version":true,"is_paid_plan":false,"start_date":1758508788374,"end_date":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)

	quota, err := client.FetchQuota(context.Background())
	testutil.NoError(t, err, "FetchQuota() error = %v")
	testutil.Equal(t, quota.PlanTier, "Free")
	testutil.False(t, quota.PaidPlan, "PaidPlan = true for a free account")
	testutil.False(t, !quota.Exhausted, "Exhausted = false, want the gateway's verdict")
	testutil.NotEqual(t, quota.UpgradeURL, "")
	testutil.Falsef(t, !quota.PeriodEnd.After(quota.SyncedAt), "PeriodEnd = %v, want a millisecond timestamp normalized to the future", quota.PeriodEnd)
}

// TestApplyQuotaStoresRemainingAsUsageCurrent pins the scheduling convention: the
// shared UsageCurrent slot carries what is LEFT, matching every other channel.
func TestApplyQuotaStoresRemainingAsUsageCurrent(t *testing.T) {
	t.Parallel()

	acc := &store.Account{AccountType: "qoder"}
	synced := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	ApplyQuota(acc, &Quota{
		Limit:      100,
		Used:       40,
		Remaining:  60,
		PlanTier:   "Pro",
		Unit:       "credits",
		UpgradeURL: "https://qoder.com/pricing?client=qoder",
		SyncedAt:   synced,
	})
	testutil.Equal(t, acc.UsageLimit, 100)
	testutil.Equal(t, acc.UsageCurrent, 60)
	testutil.Equal(t, acc.QoderQuota.PlanTier, "Pro")
	testutil.Equal(t, acc.QoderQuota.Used, 40)
	testutil.Falsef(t, !acc.QoderQuota.SyncedAt.Equal(synced), "snapshot sync time = %v, want the observation time %v", acc.QoderQuota.SyncedAt, synced)
}

// TestQuotaExhaustedWithoutNumbersIsStillExhausted proves the gateway's verdict
// wins over arithmetic: a window whose counters have not refreshed yet but which
// the upstream already flagged is still spent.
func TestQuotaExhaustedWithoutNumbersIsStillExhausted(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v2/quota/usage" {
			_, _ = w.Write([]byte(`{"isQuotaExceeded":true,"userQuota":{"total":0,"used":0,"remaining":0,"unit":"credits"}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	quota, err := client.FetchQuota(context.Background())
	testutil.NoError(t, err, "FetchQuota() error = %v")
	testutil.False(t, !quota.Exhausted, "Exhausted = false despite the upstream verdict")
}

// A missing or non-expiring reset stays unknown instead of becoming a daily
// schedule inferred from a subscription start date.
func TestReportedQuotaReset(t *testing.T) {
	for _, value := range []int64{0, -1, 253402214400000, 253402214400} {
		testutil.Falsef(t, !reportedQuotaReset(value).IsZero(), "unexpected reset for %d", value)
	}
	want := timeDate(2026, 11, 1, 0, 0, 0)
	for _, value := range []int64{want.Unix(), want.UnixMilli()} {
		testutil.Falsef(t, !reportedQuotaReset(value).Equal(want), "reset for %d differs from %v", value, want)
	}
}
