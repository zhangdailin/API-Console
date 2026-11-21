package loadbalancer

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestInvalidateAccounts_MakesChangesImmediate is the acceptance rule for the
// notification chain: a change must be visible to the NEXT request, not after the
// cache TTL expires. Before this, a deleted or disabled account could still be
// selected for up to cacheTTL.
func TestInvalidateAccounts_MakesChangesImmediate(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "pool-invalidate:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	acc := &store.Account{AccountType: "workbuddy", RefreshToken: "session-a", Enabled: true, Weight: 1}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount: %v")

	// A long TTL makes the point: without notification the pool would keep serving
	// the stale snapshot for the whole window.
	lb := NewWithCacheTTL(s, time.Hour)
	_, err = lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "workbuddy", nil, nil)
	testutil.CheckNoError(t, err)

	// Delete the account, then notify as the change bus would.
	testutil.NoError(t, s.DeleteAccount(context.Background(), acc.ID), "DeleteAccount: %v")
	lb.AccountChanges([]int64{acc.ID})

	_, err = lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "workbuddy", nil, nil)
	testutil.Error(t, err)
}

// TestInvalidateAccounts_KeepsUnrelatedAccounts guards the blast radius: only the
// changed account leaves the snapshot.
func TestInvalidateAccounts_KeepsUnrelatedAccounts(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "pool-keep:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	first := &store.Account{AccountType: "workbuddy", RefreshToken: "session-a", Enabled: true, Weight: 1}
	second := &store.Account{AccountType: "workbuddy", RefreshToken: "session-b", Enabled: true, Weight: 1}
	for _, acc := range []*store.Account{first, second} {
		testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount: %v")
	}

	lb := NewWithCacheTTL(s, time.Hour)
	_, err = lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "workbuddy", nil, nil)
	testutil.CheckNoError(t, err)

	lb.AccountChanges([]int64{first.ID})

	lb.mu.RLock()
	remaining := len(lb.cachedAccounts)
	ids := make([]int64, 0, remaining)
	for _, acc := range lb.cachedAccounts {
		ids = append(ids, acc.ID)
	}
	lb.mu.RUnlock()
	testutil.Equal(t, remaining, 1)
	testutil.Equal(t, ids[0], second.ID)
}

// TestInvalidateAccounts_WithEmptySnapshotIsSafe covers the cold path.
func TestInvalidateAccounts_WithEmptySnapshotIsSafe(t *testing.T) {
	lb := NewWithCacheTTL(nil, time.Minute)
	lb.AccountChanges([]int64{1, 2, 3})
	lb.AccountChanges(nil)
	testutil.Equal(t, len(lb.cachedAccounts), 0)
}

func TestAccountSelectionOwnsItsMutableAccount(t *testing.T) {
	initialHold := time.Now().Add(time.Hour)
	initial := &store.Account{
		ID: 1, AccountType: "workbuddy", Enabled: true,
		ModelCooldowns: map[string]time.Time{"model": initialHold},
	}
	lb := NewWithCacheTTL(&store.Store{}, time.Hour)
	lb.cachedAccounts = []*store.Account{initial}
	lb.cachedByChannel = indexAccountsByChannel(lb.cachedAccounts)
	lb.cacheExpires = time.Now().Add(time.Hour)

	selected, err := lb.GetNextAccountExcludingByChannelWithTracker(t.Context(), nil, "workbuddy", nil)
	testutil.NoError(t, err)
	selected.ModelCooldowns["model"] = initialHold.Add(time.Hour)
	lb.MarkAccountStatus(t.Context(), selected, "429")

	if initial.StatusCode != "" || initial.ModelCooldowns["model"] != initialHold {
		t.Fatal("write to a request account mutated the old cached snapshot")
	}
	if current := lb.cachedByChannel["workbuddy"][0]; current == initial || current.StatusCode != "429" || current.ModelCooldowns["model"] != initialHold {
		t.Fatal("new routing snapshot did not preserve the cached data and publish the verdict")
	}
}

func TestInvalidateAccountsDiscardsInFlightLoad(t *testing.T) {
	lb := NewWithCacheTTL(nil, time.Hour)
	read := make(chan struct{})
	resume := make(chan struct{})
	result := make(chan struct {
		rows []*store.Account
		err  error
	}, 1)
	loads := 0
	go func() {
		rows, err := lb.getOrLoadEnabledAccounts(t.Context(), func(ctx context.Context) ([]*store.Account, error) {
			loads++
			if loads == 1 {
				close(read)
				select {
				case <-resume:
					return []*store.Account{{ID: 1, AccountType: "workbuddy", Enabled: true}}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return []*store.Account{{ID: 2, AccountType: "workbuddy", Enabled: true}}, nil
		})
		result <- struct {
			rows []*store.Account
			err  error
		}{rows, err}
	}()
	<-read
	lb.InvalidateAccounts([]int64{1})
	close(resume)
	got := <-result
	testutil.NoError(t, got.err)
	if loads != 2 || len(got.rows) != 1 || got.rows[0].ID != 2 || lb.cachedByChannel["workbuddy"][0].ID != 2 {
		t.Fatalf("obsolete Redis result was returned or published after invalidation (loads=%d)", loads)
	}
}
