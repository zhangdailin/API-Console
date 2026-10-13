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
