package refreshqueue

import (
	"orchids-api/internal/testutil"
	"sync"
	"testing"
)

// TestHub_CollapsesDuplicateWork is the queue's core promise: a second refresh
// of the same account must be merged, not started while the first is running.
func TestHub_CollapsesDuplicateWork(t *testing.T) {
	hub := NewHub()
	testutil.False(t, !hub.TryAcquire(7), "first lease must be granted")
	testutil.False(t, hub.TryAcquire(7), "a second concurrent lease for the same account must be refused")
	testutil.Equal(t, hub.Len(), 1)
	hub.Release(7)
	testutil.Equal(t, hub.Len(), 0)
	testutil.False(t, !hub.TryAcquire(7), "the lease must be reusable after release")
}

// TestHub_ReleaseIsIdempotent keeps a deferred release from stealing another
// task's lease.
func TestHub_ReleaseIsIdempotent(t *testing.T) {
	hub := NewHub()
	hub.TryAcquire(1)
	hub.Release(1)
	hub.Release(1)
	testutil.False(t, !hub.TryAcquire(1), "lease must be available after a double release")
}

// TestHub_ConcurrentAcquire grants exactly one lease under contention.
func TestHub_ConcurrentAcquire(t *testing.T) {
	hub := NewHub()
	var wg sync.WaitGroup
	granted := make(chan struct{}, 16)
	for range 16 {
		wg.Go(func() {
			if hub.TryAcquire(99) {
				granted <- struct{}{}
			}
		})
	}
	wg.Wait()
	close(granted)
	count := 0
	for range granted {
		count++
	}
	testutil.Equal(t, count, 1)
}

// TestWithLease_MergesConcurrentRefreshes pins the process-wide lease shared by
// the scheduler and the manual "check" path: the second caller must not run, so
// an older snapshot cannot be written over a newer verdict.
func TestWithLease_MergesConcurrentRefreshes(t *testing.T) {
	ran := make(chan struct{}, 4)
	started := make(chan struct{})
	release := make(chan struct{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		WithLease(4242, func() {
			ran <- struct{}{}
			close(started)
			<-release
		})
	}()
	<-started

	testutil.False(t, WithLease(4242, func() { ran <- struct{}{} }), "a second lease for the same account must be refused")
	close(release)
	<-done

	testutil.Equal(t, len(ran), 1)
	// After the first lease is released the account is refreshable again.
	testutil.False(t, !WithLease(4242, func() { ran <- struct{}{} }), "the lease must be reusable once released")
	testutil.Equal(t, len(ran), 2)
}

// TestDefault_IsSharedAcrossCallers guards the wiring: the scheduler and the API
// must observe the same set, which is only true if Default() is a singleton.
func TestDefault_IsSharedAcrossCallers(t *testing.T) {
	first, second := Default(), Default()
	testutil.Equal(t, first, second)
	first.TryAcquire(99)
	t.Cleanup(func() { first.Release(99) })
	testutil.False(t, second.TryAcquire(99), "a lease taken on Default() must be visible to every caller")
}
