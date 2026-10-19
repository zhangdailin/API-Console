package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestCredentialRefreshDue(t *testing.T) {
	now := time.Now()
	base := func() *store.Account {
		return &store.Account{AccountType: "qoder", QoderAccessToken: "a", QoderRefreshToken: "r", QoderExpiresAt: now.Add(2 * time.Minute)}
	}
	cases := map[string]struct {
		mutate func(*store.Account)
		want   bool
	}{
		"expiring":           {func(*store.Account) {}, true},
		"far from expiry":    {func(a *store.Account) { a.QoderExpiresAt = now.Add(time.Hour) }, false},
		"unknown expiry":     {func(a *store.Account) { a.QoderExpiresAt = time.Time{} }, false},
		"no refresh token":   {func(a *store.Account) { a.QoderRefreshToken = "" }, false},
		"awaiting a relogin": {func(a *store.Account) { a.AuthStatus = store.AccountAuthStatusReauthRequired }, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			acc := base()
			tc.mutate(acc)
			testutil.Equal(t, credentialRefreshDue(acc, now), tc.want)
		})
	}
}

// TestMaintainRenewsAnExpiringCredential proves the scheduler rotates a token
// before a request meets it, persists the rotation, and hands the rotated
// credential to the rest of the maintenance pass.
func TestMaintainRenewsAnExpiringCredential(t *testing.T) {
	var refreshes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "deviceToken/refresh") {
			http.NotFound(w, r)
			return
		}
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_token":"access-new","refresh_token":"refresh-new","expires_in":7200}`))
	}))
	defer upstream.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "qoder-refresh-ahead:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{
		AccountType: "qoder", Enabled: true,
		QoderAccessToken: "access-old", QoderRefreshToken: "refresh-old",
		QoderExpiresAt: time.Now().Add(2 * time.Minute), QoderMachineID: "machine", QoderUserID: "uid",
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc))
	cfg := &config.Config{QoderOpenAPIBaseURL: upstream.URL, QoderOAuthBaseURL: upstream.URL, QoderInferenceURL: upstream.URL}
	env := provider.Env{Config: cfg, Store: s}

	refreshCredential(context.Background(), env, acc)
	testutil.Equal(t, refreshes.Load(), int32(1))
	testutil.Equal(t, acc.QoderAccessToken, "access-new")
	stored, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, stored.QoderRefreshToken, "refresh-new")

	// The renewed token is far from expiry: the next tick does nothing.
	refreshCredential(context.Background(), env, acc)
	testutil.Equal(t, refreshes.Load(), int32(1))
}
