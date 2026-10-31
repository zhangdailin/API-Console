package loadbalancer

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestQoderSpentAccountIsReprobedAtMostOncePerInterval: a spent account stays
// admissible to the model-aware selector so it can recover on a free model, but
// it is not a fallback on every request. Measured on 2026-11-01: a spent Free
// account was refused through two whole admission windows while a funded
// account was admitted in one, and with 41 of 44 accounts parked the fallback
// path kept re-hitting the same spent accounts.
func TestQoderSpentAccountIsReprobedAtMostOncePerInterval(t *testing.T) {
	now := time.Now()
	build := func(lastAttempt time.Time) *store.Account {
		acc := &store.Account{
			ID:          7,
			AccountType: "qoder",
			StatusCode:  store.AccountStatusQoderQuotaExhausted,
			LastAttempt: lastAttempt,
		}
		acc.QoderQuota = store.QoderQuotaSnapshot{
			Limit: 300, Used: 300, Remaining: 0, Exhausted: true,
			SyncedAt: now.Add(-time.Minute),
			ResetAt:  now.Add(time.Hour),
		}
		return acc
	}

	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	justProbed := build(now.Add(-time.Minute))
	testutil.False(t, lb.isAccountAvailable(context.Background(), justProbed),
		"a spent account probed a minute ago must not be probed again")

	lb = &LoadBalancer{connTracker: NewMemoryConnTracker()}
	due := build(now.Add(-qoderSpentProbeInterval))
	testutil.False(t, !lb.isAccountAvailable(context.Background(), due),
		"a spent account whose probe interval elapsed must be admitted")

	// A row that never attempted anything is admitted: the floor exists to stop
	// repeated probing, not to park an account before its first attempt.
	lb = &LoadBalancer{connTracker: NewMemoryConnTracker()}
	never := build(time.Time{})
	testutil.False(t, !lb.isAccountAvailable(context.Background(), never),
		"a spent account with no attempt on record must be admitted")
}
