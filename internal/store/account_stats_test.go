package store

import (
	"context"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestIncrementAccountStats_PassthroughAccountKeepsRemoteQuotaCurrent(t *testing.T) {
	t.Parallel()

	s, _ := newTestRedisStore(t, "test:")

	ctx := context.Background()
	acc := &Account{
		AccountType:  "workbuddy",
		Enabled:      true,
		UsageCurrent: 11_000_000,
		UsageLimit:   11_000_000,
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	testutil.NoError(t, s.IncrementAccountStats(ctx, acc.ID, 2048, 1), "IncrementAccountStats() error = %v")

	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, got.UsageCurrent, 11_000_000)
	testutil.Equal(t, got.UsageTotal, 2048)
	testutil.Equal(t, got.RequestCount, 1)
}

func TestIncrementAccountStats_ZeroUsageStillCountsRequest(t *testing.T) {
	t.Parallel()

	s, _ := newTestRedisStore(t, "test:")

	ctx := context.Background()
	acc := &Account{
		AccountType:  "workbuddy",
		Enabled:      true,
		UsageCurrent: 11_000_000,
		UsageTotal:   123,
		UsageLimit:   11_000_000,
	}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")

	testutil.NoError(t, s.IncrementAccountStats(ctx, acc.ID, 0, 1), "IncrementAccountStats() error = %v")

	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, got.UsageCurrent, 11_000_000)
	testutil.Equal(t, got.UsageTotal, 123)
	testutil.Equal(t, got.RequestCount, 1)
}

func TestUpdateAccount_DoesNotOverwriteAtomicUsageCounters(t *testing.T) {
	t.Parallel()

	s, _ := newTestRedisStore(t, "test:")

	ctx := context.Background()
	acc := &Account{AccountType: "qoder", Enabled: true}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	stale, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.NoError(t, s.IncrementAccountStats(ctx, acc.ID, 100, 1), "IncrementAccountStats() error = %v")
	stale.StatusCode = "429"
	testutil.NoError(t, s.UpdateAccount(ctx, stale), "UpdateAccount() error = %v")
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Falsef(t, got.UsageTotal != 100 || got.TokensToday != 100 || got.TokensDate == "", "atomic counters overwritten by stale update: total=%v today=%v date=%q", got.UsageTotal, got.TokensToday, got.TokensDate)
}

func TestIncrementAccountStats_WorkBuddyKeepsRemoteRemainingCredits(t *testing.T) {
	t.Parallel()

	s, _ := newTestRedisStore(t, "test:")

	ctx := context.Background()
	acc := &Account{AccountType: "workbuddy", Enabled: true, UsageCurrent: 0, UsageLimit: 1000}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	testutil.NoError(t, s.IncrementAccountStats(ctx, acc.ID, 500, 1), "IncrementAccountStats() error = %v")
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, got.UsageCurrent, 0)
	testutil.Equal(t, got.UsageTotal, 500)
	testutil.Equal(t, got.TokensToday, 500)
}

func TestIncrementAccountStatsOperationIsDurablyIdempotent(t *testing.T) {
	s, _ := newTestRedisStore(t, "stats-idempotent:")
	ctx := context.Background()
	acc := &Account{AccountType: "qoder", Enabled: true}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	completed := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	for range 2 {
		err := s.IncrementAccountStatsOperation(ctx, acc.ID, 42, 1, "request-123", completed)
		testutil.CheckNoError(t, err)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.UsageTotal, 42)
	testutil.Equal(t, got.RequestCount, 1)
	testutil.Equal(t, got.TokensToday, 42)
	testutil.Equal(t, got.TokensDate, "2026-03-05")
}

func TestIncrementAccountStatsOperationUsesCompletionUTCDateAcrossMidnight(t *testing.T) {
	s, _ := newTestRedisStore(t, "stats-midnight:")
	ctx := context.Background()
	acc := &Account{AccountType: "qoder", Enabled: true}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	beforeMidnight := time.Date(2026, 3, 5, 23, 59, 59, 0, time.UTC)
	afterMidnight := beforeMidnight.Add(2 * time.Second)
	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 10, 1, "newer", afterMidnight))
	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 20, 1, "delayed-older", beforeMidnight))
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.UsageTotal, 30)
	testutil.Equal(t, got.RequestCount, 2)
	testutil.Equal(t, got.TokensDate, "2026-03-06")
	testutil.Equal(t, got.TokensToday, 10)
}
