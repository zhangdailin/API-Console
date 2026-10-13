package workbuddy

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

func TestCatalogRefreshDueUsesProviderSyncTimestamp(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-provider.HealthRefreshInterval + time.Minute)
	stale := now.Add(-provider.HealthRefreshInterval - time.Minute)

	testutil.False(t, catalogRefreshDue(nil, now), "nil accounts must not be due")
	testutil.True(t, catalogRefreshDue(&store.Account{}, now), "empty snapshots must be due")
	testutil.False(t, catalogRefreshDue(&store.Account{WorkBuddyModelIDs: []string{`{"id":"m"}`}, WorkBuddyModelsSyncedAt: fresh}, now), "fresh WorkBuddy sync must not be due")
	testutil.True(t, catalogRefreshDue(&store.Account{WorkBuddyModelIDs: []string{`{"id":"m"}`}, WorkBuddyModelsSyncedAt: stale}, now), "stale WorkBuddy sync must be due")
}

func TestRefreshCatalogPersistsSuccessAndKeepsLKGOnFailure(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v3/config" {
			http.NotFound(w, r)
			return
		}
		if fail.Load() {
			http.Error(w, "temporary outage", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"fresh-model"}],"agents":[{"name":"cli","models":["fresh-model"]}]}}`))
	}))
	defer srv.Close()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "workbuddy-background:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{
		Name:                    "workbuddy",
		AccountType:             "workbuddy",
		Enabled:                 true,
		WorkBuddyAccessToken:    "access",
		WorkBuddyUID:            "uid",
		WorkBuddyModelIDs:       []string{`{"id":"last-known-good"}`},
		WorkBuddyModelsSyncedAt: time.Now().Add(-time.Hour),
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount() error = %v")
	env := provider.Env{Config: &config.Config{WorkBuddyBaseURL: srv.URL}, Store: s}
	refreshCatalog(context.Background(), env, acc)

	got, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Falsef(t, len(got.WorkBuddyModelIDs) != 1 || !strings.Contains(got.WorkBuddyModelIDs[0], `"id":"fresh-model"`) || got.WorkBuddyModelsSyncedAt.IsZero(), "successful refresh = ids %v synced_at %v", got.WorkBuddyModelIDs, got.WorkBuddyModelsSyncedAt)

	fail.Store(true)
	got.WorkBuddyModelsSyncedAt = time.Now().Add(-time.Hour)
	testutil.NoError(t, s.UpdateAccount(context.Background(), got), "make snapshot due: %v")
	refreshCatalog(context.Background(), env, got)
	afterFailure, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount(after failure) error = %v")
	testutil.Falsef(t, len(afterFailure.WorkBuddyModelIDs) != 1 || !strings.Contains(afterFailure.WorkBuddyModelIDs[0], `"id":"fresh-model"`), "failed refresh replaced LKG: %v", afterFailure.WorkBuddyModelIDs)
}

// TestQuotaRefreshDueUsesMeterTimestamp keeps the credit-meter reading on the
// same cadence as every other provider snapshot. Before this the reading was
// only written at login and on a manual check, so production held a
// 36-hour-old allowance for an account whose balance had already moved.
func TestQuotaRefreshDueUsesMeterTimestamp(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-provider.HealthRefreshInterval + time.Minute)
	stale := now.Add(-provider.HealthRefreshInterval - time.Minute)

	testutil.False(t, quotaRefreshDue(nil, now), "a nil account must not be due")
	testutil.True(t, quotaRefreshDue(&store.Account{}, now), "an account with no meter reading must be due")
	testutil.False(t, quotaRefreshDue(&store.Account{WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{SyncedAt: fresh}}, now), "a fresh reading must not be re-read on this tick")
	testutil.True(t, quotaRefreshDue(&store.Account{WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{SyncedAt: stale}}, now), "a stale reading must be re-read")
}

// TestProviderClientWritesRotatedCredentialsBack pins the reason the port hands
// the store to the constructor: a client built without it keeps a rotated
// refresh token in memory only, and the pool then presents a retired one.
func TestProviderClientWritesRotatedCredentialsBack(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "workbuddy-port:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{AccountType: "workbuddy", WorkBuddyAccessToken: "access", WorkBuddyUID: "uid"}
	withStore, ok := newProviderClient(acc, &config.Config{}, s).(*Client)
	testutil.True(t, ok, "the port must return this package's client")
	testutil.True(t, withStore.accountStore != nil, "a client built with a store must hold it")

	withoutStore := newProviderClient(acc, &config.Config{}, nil).(*Client)
	testutil.True(t, withoutStore.accountStore == nil, "a nil store must not become a non-nil interface")
}
