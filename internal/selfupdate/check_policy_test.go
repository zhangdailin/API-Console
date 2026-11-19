package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestCheckCooldownAndRecovery(t *testing.T) {
	m := fixture(t, nil)
	f := m.Source.(*fixtureSource)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	f.err = &releaseCheckError{Code: "github_rate_limited", Message: "Rate limited", RetryAt: now.Add(40 * time.Minute)}
	first := m.Check(context.Background(), false)
	if first.Available || first.ErrorCode != "github_rate_limited" || first.NextCheckAt == nil || !first.NextCheckAt.Equal(now.Add(40*time.Minute)) {
		t.Fatalf("unexpected limited check: %+v", first)
	}
	for i := 0; i < 10; i++ {
		got := m.Check(context.Background(), i%2 == 0)
		if got.Warning != first.Warning || got.Available || !got.NextCheckAt.Equal(*first.NextCheckAt) {
			t.Fatalf("cooldown lost its failure: %+v", got)
		}
	}
	if f.calls != 1 {
		t.Fatalf("cooldown made %d upstream calls", f.calls)
	}
	now = *first.NextCheckAt
	f.err = nil
	recovered := m.Check(context.Background(), true)
	if !recovered.Available || recovered.Warning != "" || recovered.ErrorCode != "" || m.checkFailures != 0 {
		t.Fatalf("failed to recover: %+v", recovered)
	}
	m.Check(context.Background(), true)
	if f.calls != 2 {
		t.Fatal("force bypassed the successful-check cooldown")
	}
	now = now.Add(time.Minute)
	m.Check(context.Background(), false)
	if f.calls != 2 {
		t.Fatal("normal check bypassed the successful cache")
	}
	m.Check(context.Background(), true)
	if f.calls != 3 {
		t.Fatal("force did not refresh after the cooldown")
	}
}

func TestCheckFailuresBackOffAndNeverRefreshStaleData(t *testing.T) {
	m := fixture(t, nil)
	f := m.Source.(*fixtureSource)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.Check(context.Background(), false)
	checked := m.checkedAt
	now = now.Add(time.Minute)
	f.err = errors.New("offline")
	for i, delay := range []time.Duration{1, 2, 4, 8, 16, 20, 20} {
		got := m.Check(context.Background(), true)
		if got.NextCheckAt == nil || !got.NextCheckAt.Equal(now.Add(delay*time.Minute)) || !got.CheckedAt.Equal(checked) || got.Warning != "offline" {
			t.Fatalf("failure %d: %+v", i, got)
		}
		if got.Available != now.Before(checked.Add(checkCacheTTL)) {
			t.Fatalf("stale cache availability was incorrect: %+v", got)
		}
		calls := f.calls
		m.Check(context.Background(), true)
		if f.calls != calls {
			t.Fatal("force bypassed failure backoff")
		}
		now = *got.NextCheckAt
	}
}

func TestConcurrentForcedChecksShareOneAttempt(t *testing.T) {
	m := fixture(t, nil)
	f := m.Source.(*fixtureSource)
	f.err = errors.New("offline")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() { m.Check(context.Background(), true) })
	}
	wg.Wait()
	if f.calls != 1 {
		t.Fatalf("concurrent checks made %d requests", f.calls)
	}
}

func TestCheckHTTPPreservesFailureAndRetryDeadline(t *testing.T) {
	m := fixture(t, nil)
	f := m.Source.(*fixtureSource)
	f.err = &releaseCheckError{Code: "github_forbidden", Message: "GitHub denied the update check (HTTP 403)."}
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		m.HandleCheck(w, httptest.NewRequest("GET", "/api/system/check-updates?force=true", nil))
		var got Check
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || got.Available || got.ErrorCode != "github_forbidden" || got.NextCheckAt == nil || got.Warning == "" {
			t.Fatalf("unexpected check response: %+v", got)
		}
	}
	if f.calls != 1 {
		t.Fatalf("HTTP force bypassed cooldown: %d", f.calls)
	}
}

type cancellableSource struct {
	*fixtureSource
	cancel context.CancelFunc
}

func (s *cancellableSource) Latest(context.Context) (Release, error) {
	s.cancel()
	return Release{}, context.Canceled
}

func TestCancelledChecksDoNotPoisonSharedFailureState(t *testing.T) {
	m := fixture(t, nil)
	f := m.Source.(*fixtureSource)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.Check(ctx, true)
	if f.calls != 0 || m.checkError != nil || !m.nextCheckAt.IsZero() {
		t.Fatal("already-cancelled caller made or cached an attempt")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	m.Source = &cancellableSource{fixtureSource: f, cancel: cancel}
	m.Check(ctx, true)
	if m.checkError != nil || m.checkFailures != 0 {
		t.Fatal("caller cancellation poisoned shared errors")
	}
	m.Source = f
	got := m.Check(context.Background(), true)
	if got.Available || got.Warning == "" || got.NextCheckAt == nil || f.calls != 0 {
		t.Fatalf("cancelled attempt bypassed the short cooldown: %+v", got)
	}
	now = now.Add(time.Minute)
	if got = m.Check(context.Background(), true); !got.Available || got.Warning != "" || f.calls != 1 {
		t.Fatalf("could not check after caller cancellation: %+v", got)
	}
}
