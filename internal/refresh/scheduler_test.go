package refresh

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	_ "orchids-api/internal/provider/all"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestMaintainAccountsToleratesWhatTheStoreCanReturn covers the rows the loop
// must step over rather than fail on: a nil entry, an account type no channel
// owns, and an account of every registered channel when there is no store to
// write to. None of them may reach an upstream or panic.
func TestMaintainAccountsToleratesWhatTheStoreCanReturn(t *testing.T) {
	accounts := []*store.Account{nil, {ID: 1, AccountType: "retired-channel", Enabled: true}}
	for index, definition := range channel.All() {
		accounts = append(accounts, &store.Account{ID: int64(index + 2), AccountType: string(definition.ID), Enabled: true})
	}
	maintainAccounts(context.Background(), provider.Env{Config: &config.Config{}}, accounts)
}

func TestMaintenanceSlowAccountDoesNotBlockOtherAccounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		accounts := []*store.Account{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}
		release := make(chan struct{})
		done := make(chan struct{})
		var completed atomic.Int32
		go func() {
			defer close(done)
			runMaintenance(context.Background(), accounts, 2, func(ctx context.Context, acc *store.Account) {
				if _, ok := ctx.Deadline(); !ok {
					t.Error("maintenance needs a bounded context")
				}
				if acc.ID == 1 {
					<-release
				}
				completed.Add(1)
			})
		}()
		synctest.Wait()
		testutil.Equal(t, completed.Load(), int32(3))
		close(release)
		<-done
		testutil.Equal(t, completed.Load(), int32(4))
	})
}

func TestMaintenanceBoundsConcurrencyAndCancelsQueuedWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var started atomic.Int32
		done := make(chan struct{})
		go func() {
			defer close(done)
			runMaintenance(ctx, []*store.Account{{ID: 1}, {ID: 2}, {ID: 3}, {ID: 4}}, 2, func(ctx context.Context, _ *store.Account) {
				started.Add(1)
				<-ctx.Done()
			})
		}()
		synctest.Wait()
		testutil.Equal(t, started.Load(), int32(2))
		cancel()
		<-done
		testutil.Equal(t, started.Load(), int32(2))
	})
}

func TestMaintenanceDeduplicatesAndIsolatesPanics(t *testing.T) {
	var completed atomic.Int32
	runMaintenance(context.Background(), []*store.Account{nil, {ID: 1}, {ID: 1}, {ID: 2}, {ID: 3}}, 1, func(_ context.Context, acc *store.Account) {
		completed.Add(1)
		if acc.ID == 1 {
			panic("test")
		}
	})
	testutil.Equal(t, completed.Load(), int32(3))
}

func TestMaintenanceAccountTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		runMaintenance(context.Background(), []*store.Account{{ID: 1}}, 1, func(ctx context.Context, _ *store.Account) { <-ctx.Done() })
		testutil.Equal(t, time.Since(started), maintenanceTimeout)
	})
}
