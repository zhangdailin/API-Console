package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestRefreshQoderQuotaClearsFalseAgentExhaustion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"userType":"personal_pro","userQuota":{"total":300,"used":82,"remaining":218,"unit":"credits"}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"user_type":"personal_pro","plan_tier_name":"Pro","is_paid_plan":true}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{StoreMode: "redis", RedisAddr: mini.Addr(), RedisPrefix: "qoder-auto-quota:"})
	if err != nil {
		t.Fatalf("store.New() error=%v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{
		AccountType: "qoder", Enabled: true, StatusCode: "402",
		StatusMessage: "agent limit", LastAttempt: time.Now().Add(-time.Hour),
		QoderAccessToken: "access", QoderRefreshToken: "refresh",
		QoderExpiresAt: time.Now().Add(6 * time.Hour), QoderMachineID: "machine",
	}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatalf("CreateAccount() error=%v", err)
	}
	cfg := &config.Config{QoderOpenAPIBaseURL: upstream.URL, QoderOAuthBaseURL: upstream.URL, QoderInferenceURL: upstream.URL}
	refreshQoderQuota(context.Background(), cfg, s, acc)

	after, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error=%v", err)
	}
	if after.StatusCode != "" || after.QoderQuota.Exhausted {
		t.Fatalf("false exhaustion survived authoritative sync: status=%q quota=%+v", after.StatusCode, after.QoderQuota)
	}
	if after.QoderQuota.Remaining != 218 || after.UsageCurrent != 218 {
		t.Fatalf("remaining=%v usage_current=%v want 218", after.QoderQuota.Remaining, after.UsageCurrent)
	}
}

func TestGrokCLIBillingNeedsSyncUsesSlowCadence(t *testing.T) {
	now := time.Now()
	if grokCLIBillingNeedsSync(&store.Account{GrokBilling: store.GrokBillingSnapshot{SyncedAt: now.Add(-time.Minute)}}, now) {
		t.Fatal("fresh CLI billing was polled on credential tick")
	}
	if !grokCLIBillingNeedsSync(&store.Account{GrokBilling: store.GrokBillingSnapshot{SyncedAt: now.Add(-time.Hour)}}, now) {
		t.Fatal("stale CLI billing was not polled")
	}
}

// TestPlanGrokRefreshCycle_SkipsAccountHoldingALease covers the write-back
// guard: an account already refreshing is merged, never scheduled twice.
