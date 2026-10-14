package cline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestBackgroundRefreshSynchronizesPlan(t *testing.T) {
	for _, fixture := range []struct {
		name, initial, body, want string
		status                    int
		paid                      bool
	}{
		{"new subscription", "free", `{"success":true,"data":{"plan":{"displayName":"Cline Pass"}}}`, "Cline Pass", http.StatusOK, true},
		{"unavailable paid plan", "Cline Pass", `{"error":"upstream down"}`, "Cline Pass", http.StatusBadGateway, true},
		{"unavailable free plan", "free", `{"error":"upstream down"}`, "free", http.StatusBadGateway, false},
		{"expired subscription", "Cline Pass", `{"error":"no plan history found for user","success":false}`, "free", http.StatusNotFound, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var planReads atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/ai/cline/recommended-models":
					_, _ = w.Write([]byte(`{"free":[{"id":"vendor/new-model"}],"clinePass":[{"id":"cline-pass/glm"}],"clineCloud":[{"id":"anthropic/cloud-only"}]}`))
				case "/users/me/plan":
					planReads.Add(1)
					w.WriteHeader(fixture.status)
					_, _ = w.Write([]byte(fixture.body))
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			mini := miniredis.RunT(t)
			s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "cline-plan-sync:"})
			testutil.NoError(t, err)
			defer func() { _ = s.Close() }()
			ctx := context.Background()
			acc := &store.Account{AccountType: "cline", Enabled: true, ClineAccessToken: "access-1", ClineExpiresAt: time.Now().Add(time.Hour),
				ClinePlan: fixture.initial, ClineModelIDs: []string{`{"id":"vendor/old-model"}`}, ClineModelsSyncedAt: time.Now().Add(-time.Hour)}
			testutil.NoError(t, s.CreateAccount(ctx, acc))
			refreshCatalog(ctx, provider.Env{Store: s, Config: &config.Config{ClineAPIBaseURL: server.URL}}, acc)
			stored, err := s.GetAccount(ctx, acc.ID)
			testutil.NoError(t, err)
			testutil.CheckEqual(t, stored.ClinePlan, fixture.want)
			testutil.True(t, CatalogSupportsModel(stored.ClineModelIDs, "vendor/new-model"))
			testutil.CheckEqual(t, (capabilities{}).SupportsModel(stored, "cline-pass/glm"), fixture.paid)
			testutil.False(t, (capabilities{}).SupportsModel(stored, "anthropic/cloud-only"))
			client := NewFromAccount(stored, nil)
			_, resolveErr := client.resolveModel(upstream.UpstreamRequest{Model: "cline-pass/glm"})
			testutil.CheckEqual(t, resolveErr == nil, fixture.paid)
			testutil.CheckEqual(t, planReads.Load(), int64(1))
			refreshCatalog(ctx, provider.Env{Store: s, Config: &config.Config{ClineAPIBaseURL: server.URL}}, stored)
			testutil.CheckEqual(t, planReads.Load(), int64(1))
			// Discovery shares the timestamp, so it must sync the plan too.
			stored.ClinePlan = fixture.initial
			observation, err := catalogPort().Observe(ctx, provider.Env{Store: s, Config: &config.Config{ClineAPIBaseURL: server.URL}}, stored)
			testutil.NoError(t, err)
			testutil.True(t, observation.Record(stored, time.Now()))
			testutil.CheckEqual(t, stored.ClinePlan, fixture.want)
			testutil.CheckEqual(t, (capabilities{}).SupportsModel(stored, "cline-pass/glm"), fixture.paid)
			testutil.CheckEqual(t, planReads.Load(), int64(2))
		})
	}
}

func TestExpiredPlanRejectsOldPaidSnapshot(t *testing.T) {
	rows := CatalogSnapshot([]Model{{ID: "vendor/free", BillingTier: "free"}, {ID: "cline-pass/glm", BillingTier: "paid"}})
	acc := &store.Account{ClinePlan: "free", ClineModelIDs: rows}
	testutil.False(t, (capabilities{}).SupportsModel(acc, "cline-pass/glm"))
	client := NewFromAccount(acc, nil)
	_, err := client.resolveModel(upstream.UpstreamRequest{Model: "cline-pass/glm"})
	testutil.Error(t, err)
	testutil.Equal(t, len(catalogPort().LastKnown(acc)), 1)
	acc.ClinePlan = "Cline Pass"
	testutil.True(t, (capabilities{}).SupportsModel(acc, "cline-pass/glm"))
}

func TestCatalogRefreshDueUsesProviderSyncTimestamp(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-provider.HealthRefreshInterval + time.Minute)
	stale := now.Add(-provider.HealthRefreshInterval - time.Minute)

	testutil.False(t, catalogRefreshDue(nil, now), "nil accounts must not be due")
	testutil.True(t, catalogRefreshDue(&store.Account{}, now), "empty snapshots must be due")
	testutil.True(t, catalogRefreshDue(&store.Account{ClineModelIDs: []string{`{"id":"m"}`}}, now), "Cline snapshot without sync timestamp must be due")
	testutil.False(t, catalogRefreshDue(&store.Account{ClineModelIDs: []string{`{"id":"m"}`}, ClineModelsSyncedAt: fresh, UpdatedAt: stale}, now), "fresh Cline sync must not become due because UpdatedAt is old")
	testutil.True(t, catalogRefreshDue(&store.Account{ClineModelIDs: []string{`{"id":"m"}`}, ClineModelsSyncedAt: stale, UpdatedAt: now}, now), "stale Cline sync must be due even when UpdatedAt is fresh")
}

// TestProviderClientHoldsTheStoreOnlyWhenGiven pins the reason the port hands
// the store to the constructor: a client built without it keeps a rotated
// refresh token in memory only, and the pool then presents a retired one.
func TestProviderClientHoldsTheStoreOnlyWhenGiven(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "cline-port:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{AccountType: "cline"}
	withStore, ok := newProviderClient(acc, &config.Config{}, s).(*Client)
	testutil.True(t, ok, "the port must return this package's client")
	testutil.True(t, withStore.accountStore != nil, "a client built with a store must hold it")

	withoutStore := newProviderClient(acc, &config.Config{}, nil).(*Client)
	testutil.True(t, withoutStore.accountStore == nil, "a nil store must not become a non-nil interface")
}
