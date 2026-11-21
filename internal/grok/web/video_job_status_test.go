package web

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func TestLookupVideoJobReadsCurrentStatusFromOtherInstances(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "video-status-test:"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler := &Handler{lb: loadbalancer.NewWithCacheTTL(s, time.Second)}
	ctx := context.Background()
	id, owner := "video_non_local_fixture", "owner-fixture"
	local := &videoJob{ID: id, OwnerHash: owner, Status: "queued", CreatedAt: time.Now().Unix()}
	putVideoJob(local)
	t.Cleanup(func() { videoJobsMu.Lock(); delete(videoJobs, id); videoJobsMu.Unlock() })
	stored := storedVideoJobFromRuntime(local)
	stored.Status, stored.Progress = "in_progress", 20
	if err := s.SaveStoredVideoJob(ctx, stored, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, ok := handler.lookupVideoJob(ctx, id, owner); !ok || got.Status != "in_progress" {
		t.Fatalf("stale first status: %+v", got)
	}
	stored.Status, stored.Progress = "completed", 100
	if err := s.SaveStoredVideoJob(ctx, stored, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, ok := handler.lookupVideoJob(ctx, id, owner); !ok || got.Status != "completed" {
		t.Fatalf("stale final status: %+v", got)
	}
	if _, ok := handler.lookupVideoJob(ctx, id, "other-owner"); ok {
		t.Fatal("lookup returned another owner's job")
	}
}
