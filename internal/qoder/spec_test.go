package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestRefreshQuotaClearsFalseAgentExhaustion(t *testing.T) {
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
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "qoder-auto-quota:"})
	testutil.NoError(t, err, "store.New() error=%v")
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{
		AccountType: "qoder", Enabled: true, StatusCode: "402",
		StatusMessage: "agent limit", LastAttempt: time.Now().Add(-time.Hour),
		QoderAccessToken: "access", QoderRefreshToken: "refresh",
		QoderExpiresAt: time.Now().Add(6 * time.Hour), QoderMachineID: "machine",
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount() error=%v")
	cfg := &config.Config{QoderOpenAPIBaseURL: upstream.URL, QoderOAuthBaseURL: upstream.URL, QoderInferenceURL: upstream.URL}
	refreshQuota(context.Background(), provider.Env{Config: cfg, Store: s}, acc)

	after, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount() error=%v")
	testutil.Falsef(t, after.StatusCode != "" || after.QoderQuota.Exhausted, "false exhaustion survived authoritative sync: status=%q quota=%+v", after.StatusCode, after.QoderQuota)
	testutil.Equal(t, after.QoderQuota.Remaining, 218)
	testutil.Equal(t, after.UsageCurrent, 218)
}

// TestProviderClientHoldsTheStoreOnlyWhenGiven pins the reason the port hands
// the store to the constructor: a client built without it keeps a rotated
// refresh token in memory only, and the pool then presents a retired one.
func TestProviderClientHoldsTheStoreOnlyWhenGiven(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "qoder-port:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{AccountType: "qoder", QoderAccessToken: "access", QoderRefreshToken: "refresh"}
	withStore, ok := newProviderClient(acc, &config.Config{}, s).(*Client)
	testutil.True(t, ok, "the port must return this package's client")
	testutil.True(t, withStore.accountStore != nil, "a client built with a store must hold it")

	withoutStore := newProviderClient(acc, &config.Config{}, nil).(*Client)
	testutil.True(t, withoutStore.accountStore == nil, "a nil store must not become a non-nil interface")
}

// A stale exhaustion verdict must recover when a fresh account-level read
// reports spendable add-on credits, including after Redis serialization.
func TestRefreshQuotaPersistsPoolsAndClearsInferredReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/quota/usage":
			_, _ = w.Write([]byte(`{"isQuotaExceeded":false,"userQuota":{"total":100,"used":100,"remaining":0},"addOnQuota":{"total":200,"used":10,"remaining":190},"orgResourcePackage":{"cap":500,"used":30,"remaining":470,"available":false}}`))
		case "/api/v2/user/plan":
			_, _ = w.Write([]byte(`{"plan_tier_name":"Pro","is_paid_plan":true,"start_date":1758508788374}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "qoder-pool-refresh:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	acc := signedTestAccount()
	acc.Enabled = true
	acc.StatusCode = store.AccountStatusQoderQuotaExhausted
	acc.QuotaResetAt = time.Now().Add(time.Hour).Truncate(time.Second)
	acc.QoderQuota = store.QoderQuotaSnapshot{Exhausted: true, ResetAt: acc.QuotaResetAt, SyncedAt: time.Now().Add(-time.Hour)}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	cfg := &config.Config{QoderOpenAPIBaseURL: server.URL, QoderOAuthBaseURL: server.URL, QoderInferenceURL: server.URL}
	refreshQuota(t.Context(), provider.Env{Config: cfg, Store: s}, acc)
	got, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.StatusCode, "")
	testutil.Equal(t, got.QoderQuota.Exhausted, false)
	testutil.Equal(t, got.UsageLimit, 300)
	testutil.Equal(t, got.UsageCurrent, 190)
	testutil.False(t, !got.QoderQuota.ResetAt.IsZero() || !got.QuotaResetAt.IsZero(), "fresh unknown boundary kept the inferred reset")
	testutil.Equal(t, got.QoderQuota.AddOnQuota.Remaining, 190)
	testutil.Equal(t, got.QoderQuota.OrgQuota.Remaining, 470)
	testutil.Equal(t, got.QoderQuota.OrgQuota.Available, false)
	fields := map[string]any{}
	projectQuota(fields, got, 0, 0, 0, false)
	pools := fields["quota_pools"].(map[string]store.QoderCreditPool)
	testutil.Equal(t, pools["add_on"].Known, true)
	testutil.Equal(t, pools["organization"].Available, false)
	testutil.EqualAny(t, fields["quota_remaining"], float64(190))
}
