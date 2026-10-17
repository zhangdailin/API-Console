package loadbalancer

import (
	"context"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type boundedCountTracker struct {
	fixedConnTracker
	maxBatch int
}

func (t *boundedCountTracker) GetCounts(ids []int64) map[int64]int64 {
	if len(ids) > t.maxBatch {
		t.maxBatch = len(ids)
	}
	return t.fixedConnTracker.GetCounts(ids)
}
func TestLargeSelectionUsesBoundedCountsAndFindsCapacity(t *testing.T) {
	accounts := make([]*store.Account, 4096)
	counts := map[int64]int64{}
	for i := range accounts {
		accounts[i] = &store.Account{ID: int64(i + 1), MaxConcurrent: 1}
		counts[int64(i+1)] = 1
	}
	counts[4096] = 0
	tracker := &boundedCountTracker{fixedConnTracker: fixedConnTracker{counts: counts}}
	lb := &LoadBalancer{}
	picked := lb.selectAccountWithTracker(accounts, tracker)
	testutil.Falsef(t, picked == nil || picked.ID != 4096 || tracker.maxBatch > accountScanWindow, "picked=%v batch=%d", picked, tracker.maxBatch)
}
func TestBatchRenewalDoesNotResurrectReleasedLease(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	tracker := NewRedisConnTracker(client, "perf:")
	defer tracker.Close()
	testutil.False(t, !tracker.TryAcquire(1, 1), "acquire")
	tracker.mu.Lock()
	id := tracker.held[1][0].id
	tracker.mu.Unlock()
	before, err := client.ZScore(context.Background(), tracker.key(1), id).Result()
	testutil.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	tracker.renewBatch()
	after, err := client.ZScore(context.Background(), tracker.key(1), id).Result()
	testutil.Falsef(t, err != nil || after <= before, "renew: %v %v", after, err)
	tracker.Release(1)
	tracker.renewBatch()
	testutil.Equal(t, tracker.GetCount(1), 0)
}
func TestCachedCountsNeverOverrideAtomicLimit(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	a := NewRedisConnTracker(client, "limit:")
	defer a.Close()
	b := NewRedisConnTracker(client, "limit:")
	defer b.Close()
	a.GetCounts([]int64{1})
	testutil.False(t, !b.TryAcquire(1, 1), "first acquire")
	testutil.False(t, a.TryAcquire(1, 1), "cached zero bypassed atomic admission")
}

type countPipelineHook struct{ commands atomic.Int64 }

func (*countPipelineHook) DialHook(next redis.DialHook) redis.DialHook          { return next }
func (*countPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook { return next }
func (h *countPipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			if cmd.Name() == "zcard" || cmd.Name() == "zremrangebyscore" {
				h.commands.Add(1)
			}
		}
		return next(ctx, cmds)
	}
}

func TestRedisCountBatchDeduplicatesAndPreservesCallerOrder(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	hook := &countPipelineHook{}
	client.AddHook(hook)
	tracker := NewRedisConnTracker(client, "dedup:")
	defer tracker.Close()
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	ids := []int64{2, 1, 2, 1}
	before := hook.commands.Load()
	got := tracker.GetCounts(ids)
	if got[1] != 0 || got[2] != 0 || len(got) != 2 {
		t.Fatalf("counts: %v", got)
	}
	if n := hook.commands.Load() - before; n != 4 {
		t.Fatalf("got %d Redis commands, want two per distinct account", n)
	}
	if ids[0] != 2 || ids[1] != 1 || ids[2] != 2 || ids[3] != 1 {
		t.Fatalf("mutated caller IDs: %v", ids)
	}
	before = hook.commands.Load()
	tracker.GetCounts([]int64{1, 2})
	if n := hook.commands.Load() - before; n != 0 {
		t.Fatalf("cache hit sent %d Redis commands", n)
	}
}
