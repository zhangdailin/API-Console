package refresh

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
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestCatalogRefreshDueUsesProviderSyncTimestamp(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-ProviderHealthRefreshInterval + time.Minute)
	stale := now.Add(-ProviderHealthRefreshInterval - time.Minute)

	testutil.False(t, clineCatalogRefreshDue(nil, now) || workBuddyCatalogRefreshDue(nil, now), "nil accounts must not be due")
	testutil.False(t, !clineCatalogRefreshDue(&store.Account{}, now) || !workBuddyCatalogRefreshDue(&store.Account{}, now), "empty snapshots must be due")
	testutil.False(t, !clineCatalogRefreshDue(&store.Account{ClineModelIDs: []string{`{"id":"m"}`}}, now), "Cline snapshot without sync timestamp must be due")
	testutil.False(t, clineCatalogRefreshDue(&store.Account{ClineModelIDs: []string{`{"id":"m"}`}, ClineModelsSyncedAt: fresh, UpdatedAt: stale}, now), "fresh Cline sync must not become due because UpdatedAt is old")
	testutil.False(t, !clineCatalogRefreshDue(&store.Account{ClineModelIDs: []string{`{"id":"m"}`}, ClineModelsSyncedAt: stale, UpdatedAt: now}, now), "stale Cline sync must be due even when UpdatedAt is fresh")
	testutil.False(t, workBuddyCatalogRefreshDue(&store.Account{WorkBuddyModelIDs: []string{`{"id":"m"}`}, WorkBuddyModelsSyncedAt: fresh}, now), "fresh WorkBuddy sync must not be due")
	testutil.False(t, !workBuddyCatalogRefreshDue(&store.Account{WorkBuddyModelIDs: []string{`{"id":"m"}`}, WorkBuddyModelsSyncedAt: stale}, now), "stale WorkBuddy sync must be due")
}

func TestRefreshWorkBuddyCatalogPersistsSuccessAndKeepsLKGOnFailure(t *testing.T) {
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
	cfg := &config.Config{WorkBuddyBaseURL: srv.URL}
	refreshWorkBuddyCatalog(context.Background(), cfg, s, acc)

	got, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Falsef(t, len(got.WorkBuddyModelIDs) != 1 || !strings.Contains(got.WorkBuddyModelIDs[0], `"id":"fresh-model"`) || got.WorkBuddyModelsSyncedAt.IsZero(), "successful refresh = ids %v synced_at %v", got.WorkBuddyModelIDs, got.WorkBuddyModelsSyncedAt)

	fail.Store(true)
	got.WorkBuddyModelsSyncedAt = time.Now().Add(-time.Hour)
	testutil.NoError(t, s.UpdateAccount(context.Background(), got), "make snapshot due: %v")
	refreshWorkBuddyCatalog(context.Background(), cfg, s, got)
	afterFailure, err := s.GetAccount(context.Background(), acc.ID)
	testutil.NoError(t, err, "GetAccount(after failure) error = %v")
	testutil.Falsef(t, len(afterFailure.WorkBuddyModelIDs) != 1 || !strings.Contains(afterFailure.WorkBuddyModelIDs[0], `"id":"fresh-model"`), "failed refresh replaced LKG: %v", afterFailure.WorkBuddyModelIDs)
}

// TestWorkBuddyQuotaRefreshDueUsesMeterTimestamp keeps the credit-meter reading
// on the same cadence as every other provider snapshot. Before this the reading
// was only written at login and on a manual check, so production held a
// 36-hour-old allowance for an account whose balance had already moved.
func TestWorkBuddyQuotaRefreshDueUsesMeterTimestamp(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-ProviderHealthRefreshInterval + time.Minute)
	stale := now.Add(-ProviderHealthRefreshInterval - time.Minute)

	testutil.False(t, workBuddyQuotaRefreshDue(nil, now), "a nil account must not be due")
	testutil.False(t, !workBuddyQuotaRefreshDue(&store.Account{}, now), "an account with no meter reading must be due")
	testutil.False(t, workBuddyQuotaRefreshDue(&store.Account{WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{SyncedAt: fresh}}, now), "a fresh reading must not be re-read on this tick")
	testutil.False(t, !workBuddyQuotaRefreshDue(&store.Account{WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{SyncedAt: stale}}, now), "a stale reading must be re-read")
}
