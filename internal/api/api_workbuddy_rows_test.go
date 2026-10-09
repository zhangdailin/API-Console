package api

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"

	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestHandleAccounts_WorkBuddyRowCarriesTierAndQuota is the end-to-end guard for
// the account table: the list endpoint must expose the plan label, the meter
// numbers and the identity the 账号/邮箱 column renders.
func TestHandleAccounts_WorkBuddyRowCarriesTierAndQuota(t *testing.T) {
	s, _ := newTestStore(t, "wb-list:")
	ctx := context.Background()

	acc := &store.Account{
		AccountType:           "workbuddy",
		Name:                  "operator@example.com",
		Email:                 "operator@example.com",
		Enabled:               true,
		Weight:                1,
		WorkBuddyAccessToken:  "access-token",
		WorkBuddyRefreshToken: "refresh-token",
		WorkBuddyUID:          "uid-abc",
		UsageLimit:            250,
		UsageCurrent:          47.28,
		WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{
			Limit:             250,
			Remaining:         47.28,
			Used:              202.72,
			LastConsumedUnits: 202,
			PackageName:       "Free Plan Subscription",
			Unit:              "credit",
			ResetAt:           time.Date(2026, 9, 26, 0, 13, 42, 0, time.UTC),
			SyncedAt:          time.Now(),
		},
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)

	var rows []map[string]interface{}
	err := json.Unmarshal(rec.Body.Bytes(), &rows)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, len(rows), 1)
	row := rows[0]
	testutil.Equal(t, row["email"], "operator@example.com")
	for key, want := range map[string]interface{}{
		"quota_plan":      "Free Plan Subscription",
		"quota_supported": true,
		"quota_limit":     250.0,
		"quota_remaining": 47.28,
	} {
		testutil.Equal(t, row[key], want)
	}
	used, ok := row["quota_used"].(float64)
	testutil.Falsef(t, !ok || math.Abs(used-202.72) > 0.01, "quota_used = %v", row["quota_used"])
	quota, ok := row["workbuddy_quota"].(map[string]interface{})
	testutil.True(t, ok, "workbuddy_quota = %v, want the meter snapshot")
	testutil.Falsef(t, quota["package_name"] != "Free Plan Subscription" || quota["synced_at"] == nil, "workbuddy_quota = %v, want the package label and a sync timestamp", quota)
	if row["workbuddy_refresh_token"] != nil {
		t.Fatalf("the refresh token leaked into the account list: %v", row["workbuddy_refresh_token"])
	}
}

// TestHandleAccounts_GrokRowCarriesSnapshotTimestamp covers the freshness signal
// the accounts page uses to decide whether a row needs an automatic re-sync:
// every channel that can report it must expose a synced_at the client can read.
func TestHandleAccounts_GrokRowCarriesSnapshotTimestamp(t *testing.T) {
	s, _ := newTestStore(t, "grok-list:")
	ctx := context.Background()
	syncedAt := time.Now().Add(-45 * time.Minute).UTC()

	oauth := &store.Account{
		AccountType:        "grok",
		CredentialType:     "oauth",
		GrokProvider:       "build",
		OAuthAccessToken:   "access",
		OAuthRefreshToken:  "refresh",
		Enabled:            true,
		GrokBilling:        store.GrokBillingSnapshot{SyncedAt: syncedAt},
		GrokModels:         []string{"grok-4.6"},
		GrokModelsSyncedAt: syncedAt,
	}
	for _, acc := range []*store.Account{oauth} {
		testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	}

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	var rows []map[string]interface{}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows), "decode: %v")
	testutil.Equal(t, len(rows), 1)
	row := rows[0]
	billing, ok := row["grok_billing"].(map[string]interface{})
	if !ok || billing["synced_at"] == nil {
		t.Fatalf("Grok Build row is missing grok_billing.synced_at: %v", row["grok_billing"])
	}
	testutil.False(t, row["grok_models_synced_at"] == nil, "Grok Build row is missing grok_models_synced_at")
}
