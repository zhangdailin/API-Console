package loadbalancer

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestMonitoringCountsReturnErrorsInsteadOfZeroLoad(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr(), MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	defer client.Close()
	tracker := NewRedisConnTracker(client, "monitor:")
	defer tracker.Close()
	if !tracker.TryAcquire(1, 2) {
		t.Fatal("could not acquire test slot")
	}
	counts, err := tracker.GetCountsChecked(context.Background(), []int64{1, 2})
	if err != nil || counts[1] != 1 || counts[2] != 0 {
		t.Fatalf("counts=%v err=%v", counts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tracker.GetCountsChecked(ctx, []int64{1}); err == nil {
		t.Fatal("canceled read appeared healthy")
	}
	mini.Close()
	if _, err := tracker.GetCountsChecked(context.Background(), []int64{1}); err == nil {
		t.Fatal("Redis outage appeared idle")
	}
}
