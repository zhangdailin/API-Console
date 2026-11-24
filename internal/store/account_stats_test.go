package store

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"orchids-api/internal/testutil"
)

func newAccountStatsTestStore(t *testing.T, prefix string) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.NewMiniRedis()
	t.Cleanup(mini.Close)
	client := redis.NewClient(&redis.Options{
		Dialer: func(context.Context, string, string) (net.Conn, error) { return mini.Dial() },
	})
	s := &Store{client: client, prefix: prefix}
	t.Cleanup(func() { _ = s.Close() })
	return s, mini
}

func TestIncrementAccountStats_PassthroughAccountKeepsRemoteQuotaCurrent(t *testing.T) {
	t.Parallel()

	s, _ := newAccountStatsTestStore(t, "test:")

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

	s, _ := newAccountStatsTestStore(t, "test:")

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

	s, _ := newAccountStatsTestStore(t, "test:")

	ctx := context.Background()
	acc := &Account{AccountType: "cline", Enabled: true}
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

	s, _ := newAccountStatsTestStore(t, "test:")

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
	s, _ := newAccountStatsTestStore(t, "stats-idempotent:")
	ctx := context.Background()
	acc := &Account{AccountType: "cline", Enabled: true}
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
	s, _ := newAccountStatsTestStore(t, "stats-midnight:")
	ctx := context.Background()
	acc := &Account{AccountType: "cline", Enabled: true}
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

func TestAccountStatsDedupExpiresWhileAccountRemainsActive(t *testing.T) {
	s, mini := newAccountStatsTestStore(t, "stats-expiry:")
	ctx := t.Context()
	acc := &Account{AccountType: "cline", Enabled: true}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	start := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	mini.SetTime(start)
	mini.ClockTTL(true)
	rs := s
	legacyKey := rs.accountStatsOperationsKey(acc.ID)
	expiryKey := rs.accountStatsExpiryKey(acc.ID)
	testutil.NoError(t, s.RedisClient().SAdd(ctx, legacyKey, "legacy-completion").Err())
	testutil.NoError(t, s.RedisClient().Expire(ctx, legacyKey, accountStatsOperationTTL).Err())

	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 999, 1, "legacy-completion", start))
	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 20, 1, "old", start))
	mid := start.Add(accountStatsOperationTTL / 2)
	mini.SetTime(mid)
	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 30, 1, "new", mid))
	later := start.Add(accountStatsOperationTTL + time.Second)
	mini.SetTime(later)
	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 40, 1, "active", later))
	testutil.NoError(t, s.IncrementAccountStatsOperation(ctx, acc.ID, 999, 1, "new", mid))

	markers, err := s.RedisClient().ZRange(ctx, expiryKey, 0, -1).Result()
	testutil.NoError(t, err)
	if len(markers) != 2 || markers[0] != "new" || markers[1] != "active" || mini.Exists(legacyKey) {
		t.Fatalf("unexpected operation markers %v, legacy still exists: %v", markers, mini.Exists(legacyKey))
	}
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.UsageTotal, 90)
	testutil.Equal(t, got.RequestCount, 3)
}

func TestAccountStatsDeletedAccountIsTerminal(t *testing.T) {
	s, _ := newAccountStatsTestStore(t, "stats-deleted:")
	acc := &Account{AccountType: "cline", Enabled: true}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	testutil.NoError(t, s.DeleteAccount(t.Context(), acc.ID))
	err := s.IncrementAccountStatsOperation(t.Context(), acc.ID, 10, 1, "pending", time.Now())
	if !errors.Is(err, ErrNoRows) {
		t.Fatalf("write to deleted account returned %v, want ErrNoRows", err)
	}
}
