package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestVideoReadOutageIsRetryableWithoutLosingOwnership(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "audit5:video:"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := &Handler{lb: loadbalancer.NewWithCacheTTL(s, time.Second)}
	const id, owner = "video_audit5_only_in_redis", "fixture-owner"
	row := &store.StoredVideoJob{ID: id, OwnerHash: owner, Status: "in_progress", Progress: 62, CreatedAt: time.Now().Unix()}
	if err := s.SaveStoredVideoJob(t.Context(), row, time.Minute); err != nil {
		t.Fatal(err)
	}
	status := func(route, scope string) int {
		r := httptest.NewRequest(http.MethodGet, "/grok/v1/videos/"+id+route, nil)
		w := httptest.NewRecorder()
		r = r.WithContext(middleware.WithAPIKeyFingerprint(r.Context(), scope))
		if route == "" {
			h.HandleVideosRetrieve(w, r)
		} else {
			h.HandleVideosContent(w, r)
		}
		return w.Code
	}
	if code := status("", owner); code != 200 {
		t.Fatalf("control GET: %d", code)
	}
	mini.SetError("temporarily unavailable")
	if code := status("", owner); code != 503 {
		t.Fatalf("expected temporary service unavailable, got %d", code)
	}
	if code := status("/content", owner); code != 503 {
		t.Fatalf("expected temporary service unavailable for content, got %d", code)
	}
	mini.SetError("")
	if code := status("", "another-owner"); code != 404 {
		t.Fatalf("unexpected other-owner status: %d", code)
	}
	if code := status("", owner); code != 200 {
		t.Fatalf("the job should still exist when Redis recovers: %d", code)
	}
}
