package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"orchids-api/internal/billingledger"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// newTestRedisStore starts an isolated in-process Redis and a Store bound to it
// under the given key prefix. The server and the store are torn down with the
// test, so a test does not spell out its own construction and cleanup.
func newTestRedisStore(t *testing.T, prefix string) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: prefix})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })
	return s, mini
}

func createBillingKey(t *testing.T, s *Store, limit int64) *ApiKey {
	t.Helper()
	key := &ApiKey{
		Name:                 "billing-" + t.Name(),
		KeyHash:              strings.ReplaceAll(t.Name(), "/", "-"),
		Enabled:              true,
		BillingLimitUSDTicks: limit,
	}
	testutil.NoError(t, s.CreateApiKey(context.Background(), key), "CreateApiKey() error = %v")
	testutil.NotEqual(t, key.ID, 0)
	return key
}

var billingTestTime = time.Now()

func billingTestEvent(name string) string { return billingledger.EventID(billingTestTime, name) }

func reserve(t *testing.T, s *Store, id int64, eventID string, amount int64, ttl time.Duration) bool {
	t.Helper()
	ok, err := s.ReserveApiKeyBilling(context.Background(), id, billingTestEvent(eventID), amount, time.Now().UTC().Add(ttl))
	testutil.Falsef(t, err != nil, "ReserveApiKeyBilling(%s, %d) error = %v", eventID, amount, err)
	return ok
}

// TestApiKeyBillingUnlimitedKeyNeverBlocks pins the zero-limit contract: a key
// created before billing limits existed must not be rationed.
func TestApiKeyBillingUnlimitedKeyNeverBlocks(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-unlimited:")
	key := createBillingKey(t, s, 0)

	for _, eventID := range []string{"event-a", "event-b", "event-c"} {
		testutil.True(t, reserve(t, s, key.ID, eventID, 1_000_000_000, time.Hour), "reservation %d of an unlimited key was refused")
	}
	got, err := s.GetApiKeyByID(context.Background(), key.ID)
	testutil.NoError(t, err, "GetApiKeyByID() error = %v")
	testutil.Equal(t, got.BillingUsedUSDTicks, 0)
}

// TestApiKeyBillingLimitBlocksExceedingReservation is the core guard: live holds
// plus settled usage may never cross the limit.
func TestApiKeyBillingLimitBlocksExceedingReservation(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-limit:")
	key := createBillingKey(t, s, 1000)

	testutil.False(t, !reserve(t, s, key.ID, "event-a", 600, time.Hour), "first reservation must succeed")
	testutil.False(t, reserve(t, s, key.ID, "event-b", 500, time.Hour), "reservation exceeding the limit must be refused")
	testutil.False(t, !reserve(t, s, key.ID, "event-b", 400, time.Hour), "reservation filling the limit exactly must succeed")
	testutil.False(t, reserve(t, s, key.ID, "event-c", 1, time.Hour), "reservation above a full limit must be refused")
	// Re-reserving the same event id with the same amount is idempotent.
	testutil.False(t, !reserve(t, s, key.ID, "event-a", 600, time.Hour), "re-reserving the same event must succeed")
	// The same event id with a different amount is a conflict, not a silent
	// second hold.
	_, err := s.ReserveApiKeyBilling(context.Background(), key.ID, billingTestEvent("event-a"), 700, time.Now().UTC().Add(time.Hour))
	testutil.Error(t, err)
}

// TestApiKeyBillingExpiredReservationsStopCounting checks that a hold whose TTL
// passed frees its capacity on the next reservation attempt.
func TestApiKeyBillingExpiredReservationsStopCounting(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-expiry:")
	key := createBillingKey(t, s, 1000)

	testutil.False(t, !reserve(t, s, key.ID, "event-expired", 900, -time.Minute), "an already-expired hold is still recorded")
	testutil.False(t, !reserve(t, s, key.ID, "event-live", 900, time.Hour), "expired capacity must be reclaimed")
	testutil.False(t, reserve(t, s, key.ID, "event-extra", 200, time.Hour), "only the live hold may count")
}

func TestApiKeyBillingSettlementIsIdempotentByEvent(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-idempotent:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("same-event"), 300))
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("same-event"), 300), "idempotent replay failed: %v")
	got, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.BillingUsedUSDTicks, 300)
	err = s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("same-event"), 301)
	testutil.Error(t, err)
}

// TestApiKeyBillingSettleMovesReservationIntoUsed pins settlement: the hold
// disappears, the charge lands in the used counter, and actual usage is billed
// even when its hold is gone.
func TestApiKeyBillingSettleMovesReservationIntoUsed(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-settle:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()

	testutil.False(t, !reserve(t, s, key.ID, "event-a", 400, time.Hour), "reservation must succeed")
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("event-a"), 300), "SettleApiKeyBilling() error = %v")
	// The hold is gone, so its capacity is neither free nor double counted.
	testutil.False(t, !reserve(t, s, key.ID, "event-b", 700, time.Hour), "used 300 + held 700 must fit the limit")
	testutil.False(t, reserve(t, s, key.ID, "event-c", 1, time.Hour), "limit must now be full")

	released, err := s.ReleaseApiKeyBilling(ctx, key.ID, billingTestEvent("event-a"))
	testutil.NoError(t, err, "ReleaseApiKeyBilling() error = %v")
	testutil.False(t, released, "a settled reservation must not be released again")

	// Settling an event whose hold already expired still charges: the request ran.
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("event-unknown"), 100), "settling an unknown event error = %v")
	got, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err, "GetApiKeyByID() error = %v")
	testutil.Equal(t, got.BillingUsedUSDTicks, 400)
	listed, err := s.ListApiKeys(ctx)
	testutil.NoError(t, err, "ListApiKeys() error = %v")
	testutil.Equal(t, len(listed), 1)
	testutil.Equal(t, listed[0].BillingUsedUSDTicks, 400)
	testutil.Equal(t, listed[0].BillingLimitUSDTicks, 1000)
}

// TestApiKeyBillingReleaseFreesCapacityAndReportsExistence covers the release
// path the request middleware relies on to avoid charging a failed request.
func TestApiKeyBillingReleaseFreesCapacityAndReportsExistence(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-release:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()

	testutil.False(t, !reserve(t, s, key.ID, "event-a", 400, time.Hour), "reservation must succeed")
	released, err := s.ReleaseApiKeyBilling(ctx, key.ID, billingTestEvent("event-a"))
	testutil.Falsef(t, err != nil || !released, "ReleaseApiKeyBilling() = %v, %v; want true, nil", released, err)
	testutil.False(t, !reserve(t, s, key.ID, "event-b", 1000, time.Hour), "released capacity must be reusable")
	released, err = s.ReleaseApiKeyBilling(ctx, key.ID, billingTestEvent("event-a"))
	testutil.Falsef(t, err != nil || released, "second release = %v, %v; want false, nil", released, err)
}

// TestApiKeyBillingResetZeroesUsedAndDropsReservations covers the admin reset.
func TestApiKeyBillingResetZeroesUsedAndDropsReservations(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-reset:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()

	testutil.False(t, !reserve(t, s, key.ID, "event-a", 400, time.Hour), "reservation must succeed")
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("event-a"), 400), "SettleApiKeyBilling() error = %v")
	testutil.NoError(t, s.ResetApiKeyBilling(ctx, key.ID), "ResetApiKeyBilling() error = %v")
	got, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err, "GetApiKeyByID() error = %v")
	testutil.Equal(t, got.BillingUsedUSDTicks, 0)
	testutil.Equal(t, got.BillingLimitUSDTicks, 1000)
	testutil.False(t, !reserve(t, s, key.ID, "event-b", 1000, time.Hour), "a full limit must be available after a reset")
}

// TestApiKeyBillingLimitMirrorFollowsKeyUpdates checks the Redis limit mirror is
// rewritten by UpdateApiKey, which is what the admin PATCH path uses.
func TestApiKeyBillingLimitMirrorFollowsKeyUpdates(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-mirror:")
	key := createBillingKey(t, s, 0)
	ctx := context.Background()

	testutil.False(t, !reserve(t, s, key.ID, "event-unlimited", 10_000, time.Hour), "a key without a limit must not be rationed")
	_, err := s.ReleaseApiKeyBilling(ctx, key.ID, billingTestEvent("event-unlimited"))
	testutil.CheckNoError(t, err)

	key.BillingLimitUSDTicks = 100
	testutil.NoError(t, s.UpdateApiKey(ctx, key), "UpdateApiKey() error = %v")
	testutil.False(t, reserve(t, s, key.ID, "event-limited", 200, time.Hour), "the updated limit must be enforced immediately")
	testutil.False(t, !reserve(t, s, key.ID, "event-fits", 100, time.Hour), "the updated limit must still admit a fitting request")

	// Deleting the key drops its ledger keys with it.
	testutil.NoError(t, s.DeleteApiKey(ctx, key.ID), "DeleteApiKey() error = %v")
	_, err = s.GetApiKeyByID(ctx, key.ID)
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "GetApiKeyByID() after delete = %v, want ErrNoRows", err)
}

// TestApiKeyBillingRejectsInvalidReservations pins the argument validation so a
// caller mistake cannot create an unbounded hold.
func TestApiKeyBillingRejectsInvalidReservations(t *testing.T) {
	s, _ := newTestRedisStore(t, "billing-invalid:")
	ctx := context.Background()
	key := createBillingKey(t, s, 1000)

	_, err := s.ReserveApiKeyBilling(ctx, key.ID, "", 10, time.Now().Add(time.Hour))
	testutil.Error(t, err)
	_, err = s.ReserveApiKeyBilling(ctx, key.ID, billingTestEvent("event"), 0, time.Now().Add(time.Hour))
	testutil.Error(t, err)
	_, err = s.ReserveApiKeyBilling(ctx, key.ID, billingTestEvent("event"), 10, time.Time{})
	testutil.Error(t, err)
	_, err = s.ReserveApiKeyBilling(ctx, 0, billingTestEvent("event"), 10, time.Now().Add(time.Hour))
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "reserving for key 0 = %v, want ErrNoRows", err)
	err = s.SettleApiKeyBilling(ctx, 0, billingTestEvent("event"), 10)
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "settling key 0 = %v, want ErrNoRows", err)
	err = s.ResetApiKeyBilling(ctx, 0)
	testutil.Falsef(t, !errors.Is(err, ErrNoRows), "resetting key 0 = %v, want ErrNoRows", err)
}

// A key with a billing period starts a fresh period once it elapses, so a limit
// is per period rather than forever.
func TestApiKeyBillingPeriodRollsOver(t *testing.T) {
	s, _ := newTestRedisStore(t, "period:")
	ctx := context.Background()

	now := time.Now().UTC()
	key := &ApiKey{
		Name: "period", KeyHash: "hash-period", Enabled: true,
		BillingLimitUSDTicks: 1_000_000, BillingPeriodDays: 1,
		BillingPeriodStartedAt: now.Add(-48 * time.Hour),
	}
	testutil.NoError(t, s.CreateApiKey(ctx, key), "CreateApiKey: %v")
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("req_old"), 500_000), "SettleApiKeyBilling: %v")
	stored, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, stored.BillingUsedUSDTicks, 500_000)

	// Authorizing the key rolls an elapsed period over. The lookup hashes the raw
	// value, so the key is created with the hash of the raw value the caller uses.
	raw := "raw-period-key"
	digest := sha256.Sum256([]byte(raw))
	key.KeyHash = hex.EncodeToString(digest[:])
	testutil.NoError(t, s.UpdateApiKey(ctx, key), "UpdateApiKey: %v")
	_, err = s.AuthorizeApiKey(ctx, raw)
	testutil.CheckNoError(t, err)
	rolled, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err, "GetApiKeyByID: %v")
	testutil.Equal(t, rolled.BillingUsedUSDTicks, 0)
	testutil.Falsef(t, !rolled.BillingPeriodStartedAt.After(now.Add(-time.Minute)), "period start was not advanced: %v", rolled.BillingPeriodStartedAt)
}

func TestApiKeyBillingPeriodRolloverPreservesLiveHoldsAndIsAtomic(t *testing.T) {
	s, _ := newTestRedisStore(t, "period-atomic:")
	ctx := context.Background()
	now := time.Now().UTC()
	raw := "raw-period-atomic"
	digest := sha256.Sum256([]byte(raw))
	key := &ApiKey{
		Name: "period atomic", KeyHash: hex.EncodeToString(digest[:]), Enabled: true,
		BillingLimitUSDTicks: 1_000, BillingPeriodDays: 1,
		BillingPeriodStartedAt: now.Add(-48 * time.Hour),
	}
	testutil.NoError(t, s.CreateApiKey(ctx, key))
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("old"), 900))
	ok, err := s.ReserveApiKeyBilling(ctx, key.ID, billingTestEvent("live"), 100, now.Add(time.Hour))
	testutil.Falsef(t, err != nil || !ok, "live hold: ok=%v err=%v", ok, err)
	// Two stale snapshots model concurrent authorizations. Exactly one can match
	// and advance the durable period start; the second must not reset fresh usage.
	staleA, _ := s.GetApiKeyByID(ctx, key.ID)
	staleB := *staleA
	s.rolloverApiKeyBilling(ctx, staleA, now)
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("fresh"), 50))
	s.rolloverApiKeyBilling(ctx, &staleB, now.Add(time.Second))

	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("live"), 100), "live hold was deleted by rollover: %v")
	got, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.BillingUsedUSDTicks, 150)
	released, err := s.ReleaseApiKeyBilling(ctx, key.ID, billingTestEvent("live"))
	testutil.Falsef(t, err != nil || released, "settled live hold remains: released=%v err=%v", released, err)
}

// Resetting billing by hand zeroes the counter without touching the limit.
func TestResetApiKeyBillingKeepsTheLimit(t *testing.T) {
	s, _ := newTestRedisStore(t, "reset:")
	ctx := context.Background()

	key := &ApiKey{Name: "reset", KeyHash: "hash-reset", Enabled: true, BillingLimitUSDTicks: 2_000_000}
	testutil.NoError(t, s.CreateApiKey(ctx, key), "CreateApiKey: %v")
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingTestEvent("req_1"), 1_000_000), "SettleApiKeyBilling: %v")
	testutil.NoError(t, s.ResetApiKeyBilling(ctx, key.ID), "ResetApiKeyBilling: %v")
	stored, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err, "GetApiKeyByID: %v")
	testutil.Equal(t, stored.BillingUsedUSDTicks, 0)
	testutil.Equal(t, stored.BillingLimitUSDTicks, 2_000_000)
	// Capacity is back: a reservation that the old usage would have blocked now
	// succeeds.
	ok, err := s.ReserveApiKeyBilling(ctx, key.ID, billingTestEvent("req_2"), 2_000_000, time.Now().Add(time.Minute))
	testutil.Falsef(t, err != nil || !ok, "reserve after reset ok=%v err=%v", ok, err)
}
