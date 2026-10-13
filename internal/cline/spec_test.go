package cline

import (
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
