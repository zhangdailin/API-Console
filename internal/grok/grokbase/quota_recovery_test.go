package grokbase

import (
	"testing"
	"time"

	"orchids-api/internal/testutil"
)

func TestRecoveryQueueClaimsOnceThenBacksOff(t *testing.T) {
	var q RecoveryQueue
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

	testutil.True(t, q.Claim(7, now), "a spent account is probed on first sight")
	testutil.False(t, q.Claim(7, now.Add(time.Second)), "a claimed account is leased")

	due := q.Reschedule(7, now, time.Time{})
	testutil.Equal(t, due, now.Add(RecoveryBackoffBase))
	testutil.False(t, q.Claim(7, due.Add(-time.Second)))
	testutil.True(t, q.Claim(7, due))

	due = q.Reschedule(7, now, time.Time{})
	testutil.Equal(t, due, now.Add(2*RecoveryBackoffBase))

	// A stated reset wins over the backoff.
	reset := now.Add(3 * time.Hour)
	testutil.Equal(t, q.Reschedule(7, now, reset), reset)

	q.Done(7)
	testutil.False(t, q.Pending(7))
	testutil.True(t, q.Claim(7, now))
}

func TestRecoveryBackoffIsBounded(t *testing.T) {
	testutil.Equal(t, RecoveryBackoff(1), RecoveryBackoffBase)
	testutil.Equal(t, RecoveryBackoff(3), 4*RecoveryBackoffBase)
	testutil.Equal(t, RecoveryBackoff(50), RecoveryBackoffMax)
}
