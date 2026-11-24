package loadbalancer

import (
	"context"
	"orchids-api/internal/testutil"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisConnTrackerLeaseIsAtomicSharedAndReleased(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()

	first := NewRedisConnTracker(client, "test:")
	second := NewRedisConnTracker(client, "test:")
	release, ok := first.Reserve(t.Context(), 42, 1)
	testutil.False(t, !ok, "first lease was rejected")
	_, ok = second.Reserve(t.Context(), 42, 1)
	testutil.False(t, ok, "second process exceeded the shared hard limit")
	testutil.Equal(t, second.GetCount(42), 1)
	// Constructing another tracker must never erase live leases from a peer.
	third := NewRedisConnTracker(client, "test:")
	testutil.Equal(t, third.GetCount(42), 1)
	release()
	testutil.Equal(t, second.GetCount(42), 0)
}

func TestRedisConnTrackerReclaimsExpiredAndLegacyCounters(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	tracker := NewRedisConnTracker(client, "test:")
	key := tracker.key(7)

	testutil.NoError(t, client.ZAdd(context.Background(), key, redis.Z{Score: float64(time.Now().Add(-time.Minute).UnixMilli()), Member: "dead"}).Err())
	testutil.Equal(t, tracker.GetCount(7), 0)

	testutil.NoError(t, client.Set(context.Background(), key, "99", 0).Err())
	release, ok := tracker.Reserve(t.Context(), 7, 1)
	testutil.False(t, !ok, "legacy string counter prevented lease migration")
	testutil.Equal(t, tracker.GetCount(7), 1)
	release()
}

func TestRedisConnTrackerCloseReleasesOwnedLeases(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()

	tracker := NewRedisConnTracker(client, "test:")
	peer := NewRedisConnTracker(client, "test:")
	_, trackerOK := tracker.Reserve(t.Context(), 42, 2)
	release, peerOK := peer.Reserve(t.Context(), 42, 2)
	testutil.False(t, !trackerOK || !peerOK, "failed to acquire owned leases")
	tracker.Close()
	testutil.Equal(t, peer.GetCount(42), 1)
	release()
	_, ok := tracker.Reserve(t.Context(), 42, 1)
	testutil.False(t, ok, "closed tracker acquired a new lease")
}

func TestRedisConnTrackerCancelsOnlyTheLostRequest(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	tracker := NewRedisConnTracker(client, "lost:")
	defer tracker.Close()
	a, closeA := WithConnLeaseScope(t.Context())
	defer closeA()
	b, closeB := WithConnLeaseScope(t.Context())
	defer closeB()
	x, ok := ReserveConnection(a, tracker, 42, 2)
	if !ok {
		t.Fatal("first reservation failed")
	}
	defer x()
	y, ok := ReserveConnection(b, tracker, 42, 2)
	if !ok {
		t.Fatal("second reservation failed")
	}
	defer y()
	tracker.mu.Lock()
	lost := tracker.held[42][0].id
	tracker.mu.Unlock()

	mini.SetError("temporarily unavailable")
	tracker.renewBatch()
	if a.Err() != nil || b.Err() != nil {
		t.Fatal("a transient error canceled a still-valid lease")
	}
	mini.SetError("")
	if err := client.ZRem(t.Context(), tracker.key(42), lost).Err(); err != nil {
		t.Fatal(err)
	}
	tracker.renewBatch()
	if context.Cause(a) != ErrConnectionLeaseLost || b.Err() != nil {
		t.Fatalf("incorrect request cancellation: first=%v second=%v", context.Cause(a), context.Cause(b))
	}
	x()
	if n := client.ZCard(t.Context(), tracker.key(42)).Val(); n != 1 {
		t.Fatalf("releasing a lost lease released its peer: %d", n)
	}
	y()
	if b.Err() != nil {
		t.Fatal("normal release canceled the logical request")
	}
}

func TestRedisConnTrackerStopsAtLastProvenDeadlineDuringOutage(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	tracker := NewRedisConnTracker(client, "deadline:")
	defer tracker.Close()
	ctx, closeScope := WithConnLeaseScope(t.Context())
	defer closeScope()
	release, ok := ReserveConnection(ctx, tracker, 1, 1)
	if !ok {
		t.Fatal("initial reservation failed")
	}
	defer release()

	mini.SetError("temporarily unavailable")
	tracker.renewBatch()
	tracker.mu.Lock()
	lease := tracker.held[1][0]
	lease.expires = time.Now().Add(-time.Millisecond)
	lease.timer.Reset(time.Millisecond)
	tracker.mu.Unlock()
	select {
	case <-ctx.Done():
		if context.Cause(ctx) != ErrConnectionLeaseLost {
			t.Fatalf("wrong cause: %v", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Fatal("request continued after the last proven deadline")
	}
	mini.SetError("")
}
