package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

type videoRestoreTransport func(*http.Request) (*http.Response, error)

func (f videoRestoreTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCompletedVideoCanDownloadFromAnotherReplica(t *testing.T) {
	mini := miniredis.RunT(t)
	st, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "video-download:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	oldRoot := cacheBaseDir
	cacheBaseDir = t.TempDir()
	t.Cleanup(func() { cacheBaseDir = oldRoot })
	const origin = "https://videos.x.ai/fixture.mp4"
	var fetched atomic.Int32
	client := &Client{httpClient: &http.Client{Transport: videoRestoreTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != origin || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected asset request: %v", r.URL.Host)
		}
		fetched.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"video/mp4"}}, ContentLength: int64(len("replica-video")), Body: io.NopCloser(strings.NewReader("replica-video"))}, nil
	})}}
	h := &Handler{lb: loadbalancer.NewWithCacheTTL(st, time.Second), client: client}
	save := func(row *store.StoredVideoJob) {
		t.Helper()
		row.OwnerHash, row.Status, row.CreatedAt = "anonymous", "completed", time.Now().Unix()
		if err := st.SaveStoredVideoJob(t.Context(), row, time.Hour); err != nil {
			t.Fatal(err)
		}
	}
	save(&store.StoredVideoJob{ID: "video_origin", Provider: "build", VideoURL: origin, ContentPath: "/different/mount/video/missing.mp4"})
	save(&store.StoredVideoJob{ID: "video_upload", Provider: "build", ContentPath: "/different/mount/video/upload.mp4", ContentName: "upload.mp4"})
	if err := os.MkdirAll(filepath.Join(cacheBaseDir, "video"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheBaseDir, "video", "upload.mp4"), []byte("shared-video"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, want string }{{"video_origin", "rep"}, {"video_upload", "sha"}, {"video_origin", "rep"}} {
		id, want := tc.id, tc.want
		r := httptest.NewRequest(http.MethodGet, "/grok/v1/videos/"+id+"/content", nil)
		r.Header.Set("Range", "bytes=0-2")
		rec := httptest.NewRecorder()
		h.HandleVideosContent(rec, r)
		if rec.Code != http.StatusPartialContent || rec.Body.String() != want {
			t.Fatalf("%s: HTTP %d, %s", id, rec.Code, rec.Body.String())
		}
	}
	if fetched.Load() != 1 {
		t.Fatalf("expected one origin fetch, got %d", fetched.Load())
	}
}

func TestRecoveryDoesNotTakeOverAJobCompletedSinceListing(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "video-recovery-state:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := t.Context()
	row := &store.StoredVideoJob{ID: "fixture", OwnerHash: "owner", Status: "in_progress", CreatedAt: time.Now().Add(-3 * time.Minute).Unix()}
	if err := s.SaveStoredVideoJob(ctx, row, time.Hour); err != nil {
		t.Fatal(err)
	}
	const original = "original-worker"
	if ok, err := s.AcquireVideoJobLease(ctx, row.ID, row.OwnerHash, original, time.Second); err != nil || !ok {
		t.Fatalf("original lease: %v, %v", ok, err)
	}
	list, err := s.ListStoredVideoJobs(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("listed jobs: %d, %v", len(list), err)
	}
	mini.FastForward(2 * time.Second)
	const next = "new-worker"
	if ok, err := s.AcquireVideoJobLease(ctx, row.ID, row.OwnerHash, next, time.Minute); err != nil || !ok {
		t.Fatalf("new worker lease: %v, %v", ok, err)
	}
	if err := s.SaveLeasedVideoJob(ctx, row, time.Hour, original); !errors.Is(err, store.ErrVideoJobLeaseLost) {
		t.Fatalf("an expired lease could still write: %v", err)
	}
	row.Status = "completed"
	if err := s.SaveLeasedVideoJob(ctx, row, time.Hour, next); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveLeasedVideoJob(ctx, list[0], time.Hour, next); !errors.Is(err, store.ErrVideoJobLeaseLost) {
		t.Fatalf("active lease could overwrite terminal status: %v", err)
	}
	if ok, err := s.ReleaseVideoJobLease(ctx, row.ID, row.OwnerHash, next); err != nil || !ok {
		t.Fatalf("release worker lease: %v, %v", ok, err)
	}
	h := &Handler{lb: loadbalancer.NewWithCacheTTL(s, time.Second), instanceID: "recovery"}
	h.takeOverVideoJob(ctx, list[0])
	got, err := s.GetStoredVideoJob(ctx, row.ID, row.OwnerHash)
	if err != nil || got.Status != "completed" || videoRunningHere(row.ID) {
		t.Fatalf("recovery touched completed video: %+v, %v", got, err)
	}
}

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
	local := &videoJob{StoredVideoJob: store.StoredVideoJob{ID: id, OwnerHash: owner, Status: "queued", CreatedAt: time.Now().Unix()}}
	putVideoJob(local)
	t.Cleanup(func() { videoJobsMu.Lock(); delete(videoJobs, id); videoJobsMu.Unlock() })
	stored := storedVideoJobFromRuntime(local)
	stored.Status, stored.Progress = "in_progress", 20
	if err := s.SaveStoredVideoJob(ctx, stored, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := handler.lookupVideoJob(ctx, id, owner); err != nil || got.Status != "in_progress" {
		t.Fatalf("stale first status: %+v", got)
	}
	stored.Status, stored.Progress = "completed", 100
	if err := s.SaveStoredVideoJob(ctx, stored, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got, err := handler.lookupVideoJob(ctx, id, owner); err != nil || got.Status != "completed" {
		t.Fatalf("stale final status: %+v", got)
	}
	if _, err := handler.lookupVideoJob(ctx, id, "other-owner"); err != store.ErrNoRows {
		t.Fatal("lookup returned another owner's job")
	}
}
