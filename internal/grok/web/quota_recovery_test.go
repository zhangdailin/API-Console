package web

import (
	"errors"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestWebQuotaRecoveryReleasesAnAccountWhenTheWindowRefills(t *testing.T) {
	now := time.Now()
	acc := &store.Account{ID: 9101, StatusCode: "429", StatusMessage: "limit", RateLimitFailures: 3}
	acc.GrokWebQuota.SyncedAt = now
	acc.GrokWebQuota.Fast = store.GrokQuotaWindow{HasRemaining: true, Remaining: 0, ResetAt: now.Add(time.Hour)}
	t.Cleanup(func() { webQuotaRecovery.Done(acc.ID) })

	testutil.True(t, claimWebQuotaRecovery(acc, now))
	// Still spent: rescheduled at the stated reset, not probed before it.
	settleWebQuotaRecovery(acc, "", nil, now)
	testutil.Equal(t, acc.StatusCode, "429")
	testutil.False(t, claimWebQuotaRecovery(acc, now.Add(30*time.Minute)))
	testutil.True(t, claimWebQuotaRecovery(acc, now.Add(time.Hour)))

	// A failed read proves nothing and backs off.
	settleWebQuotaRecovery(acc, "", errors.New("timeout"), now.Add(time.Hour))
	testutil.Equal(t, acc.StatusCode, "429")

	acc.GrokWebQuota.Fast.Remaining = 20
	settleWebQuotaRecovery(acc, "", nil, now.Add(2*time.Hour))
	testutil.Equal(t, acc.StatusCode, "")
	testutil.Equal(t, acc.RateLimitFailures, 0)
	testutil.False(t, webQuotaRecovery.Pending(acc.ID))
}

func TestWebQuotaRecoveryIgnoresABareThrottle(t *testing.T) {
	now := time.Now()
	acc := &store.Account{ID: 9102, StatusCode: "429"}
	acc.GrokWebQuota.Fast = store.GrokQuotaWindow{HasRemaining: true, Remaining: 5}
	testutil.False(t, claimWebQuotaRecovery(acc, now))
}

func TestWebQuotaSpentDerivesTheResetFromTheWindowLength(t *testing.T) {
	now := time.Now()
	acc := &store.Account{}
	acc.GrokWebQuota.SyncedAt = now
	acc.GrokWebQuota.Auto = store.GrokQuotaWindow{HasRemaining: true, Remaining: 0, WindowSeconds: 7200}
	spent, reset := webQuotaSpent(acc, now)
	testutil.True(t, spent)
	testutil.Equal(t, reset, now.Add(2*time.Hour))
}
