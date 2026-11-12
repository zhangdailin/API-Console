package grok

import (
	"errors"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestBuildQuotaRecoveryReleasesOnlyOnAnObservedAllowance(t *testing.T) {
	now := time.Now()
	acc := &store.Account{ID: 9201, StatusCode: "402", StatusMessage: "credits spent", LastAttempt: now}
	t.Cleanup(func() { buildQuotaRecovery.Done(acc.ID) })
	testutil.True(t, claimBuildQuotaRecovery(acc, now))

	// A failed billing read keeps the account parked.
	settleBuildQuotaRecovery(acc, errors.New("timeout"), now)
	testutil.Equal(t, acc.StatusCode, "402")
	testutil.True(t, buildQuotaRecovery.Pending(acc.ID))

	// An exhausted snapshot reschedules at the period end.
	periodEnd := now.Add(6 * time.Hour)
	acc.GrokBilling = store.GrokBillingSnapshot{SyncedAt: now, Weekly: store.GrokQuotaWindow{HasUsage: true, UsagePercent: 100, ResetAt: periodEnd}}
	settleBuildQuotaRecovery(acc, nil, now)
	testutil.Equal(t, acc.StatusCode, "402")
	testutil.False(t, claimBuildQuotaRecovery(acc, now.Add(5*time.Hour)))
	testutil.True(t, claimBuildQuotaRecovery(acc, periodEnd))

	acc.GrokBilling.Weekly.UsagePercent = 12
	settleBuildQuotaRecovery(acc, nil, periodEnd)
	testutil.Equal(t, acc.StatusCode, "")
	testutil.True(t, acc.LastAttempt.IsZero())
	testutil.False(t, buildQuotaRecovery.Pending(acc.ID))
}

func TestBuildQuotaRecoveryNeedsABillingSignal(t *testing.T) {
	now := time.Now()
	acc := &store.Account{ID: 9202, StatusCode: "402"}
	t.Cleanup(func() { buildQuotaRecovery.Done(acc.ID) })
	testutil.True(t, claimBuildQuotaRecovery(acc, now))
	// A synced snapshot without any allowance number is not evidence.
	acc.GrokBilling = store.GrokBillingSnapshot{SyncedAt: now}
	settleBuildQuotaRecovery(acc, nil, now)
	testutil.Equal(t, acc.StatusCode, "402")

	acc.StatusCode = ""
	testutil.False(t, claimBuildQuotaRecovery(acc, now.Add(time.Hour)))
	testutil.False(t, buildQuotaRecovery.Pending(acc.ID))
}
