package store

import (
	"context"
	"orchids-api/internal/testutil"
	"runtime"
	"sync"
	"testing"
	"time"
)

// recordingEmitter captures the notifications the store publishes.
type recordingEmitter struct {
	mu      sync.Mutex
	changes []AccountChange
}

func (e *recordingEmitter) Publish(change AccountChange) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.changes = append(e.changes, change)
}

// waitForChanges lets the asynchronous emitter settle before assertions.
func (e *recordingEmitter) waitForChanges(t *testing.T, want int) []AccountChange {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if changes := e.all(); len(changes) >= want {
			return changes
		}
		time.Sleep(2 * time.Millisecond)
	}
	return e.all()
}

func (e *recordingEmitter) all() []AccountChange {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]AccountChange(nil), e.changes...)
}

func newEmitterStore(t *testing.T) (*Store, *recordingEmitter) {
	t.Helper()
	s, _ := newTestRedisStore(t, "events:")
	emitter := &recordingEmitter{}
	s.SetChangeEmitter(emitter)
	return s, emitter
}

// TestChangeEmitter_PublishesOnlyAfterAPersistedWrite pins the contract the whole
// notification chain rests on: no event for a write that did not happen.
func TestChangeEmitter_PublishesOnlyAfterAPersistedWrite(t *testing.T) {
	s, emitter := newEmitterStore(t)
	ctx := context.Background()

	acc := &Account{AccountType: "cline", RefreshToken: "session-a", Enabled: true, Weight: 1}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount: %v")
	changes := emitter.waitForChanges(t, 1)
	testutil.Equal(t, len(changes), 1)
	testutil.Equal(t, changes[0].AccountID, acc.ID)
	testutil.Equal(t, changes[0].Previous, nil)

	// An update carries the previous state, which is what lets a subscriber see
	// whether the credential moved.
	updated, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount: %v")
	updated.RefreshToken = "session-b"
	testutil.NoError(t, s.UpdateAccount(ctx, updated), "UpdateAccount: %v")
	changes = emitter.waitForChanges(t, 2)
	testutil.Equal(t, len(changes), 2)
	if changes[1].Previous == nil || changes[1].Previous.RefreshToken != "session-a" {
		t.Fatalf("update change lost the previous state: %+v", changes[1])
	}
	// The store publishes the id and the previous state; the after-state is the
	// emitter's to resolve, so it is not asserted here.
	testutil.Equal(t, changes[1].AccountID, acc.ID)

	// Deleting publishes once, and deleting a missing id publishes nothing.
	testutil.NoError(t, s.DeleteAccount(ctx, acc.ID), "DeleteAccount: %v")
	testutil.Equal(t, len(emitter.waitForChanges(t, 3)), 3)
	testutil.NoError(t, s.DeleteAccount(ctx, acc.ID), "second DeleteAccount: %v")
	testutil.Equal(t, len(emitter.waitForChanges(t, 3)), 3)
}

// TestChangeEmitter_SilentWhenNoEmitterConfigured keeps a plain store (tests, a
// deployment without the bus) working.
func TestChangeEmitter_SilentWhenNoEmitterConfigured(t *testing.T) {
	s, _ := newTestRedisStore(t, "silent:")

	acc := &Account{AccountType: "cline", RefreshToken: "x", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount with no emitter: %v")
	testutil.NoError(t, s.DeleteAccount(context.Background(), acc.ID), "DeleteAccount with no emitter: %v")
}

// TestChangeEmitter_DoesNotBlockTheWrite keeps observability out of the write
// path: a slow emitter must not turn a successful write into a failed one.
func TestChangeEmitter_DoesNotBlockTheWrite(t *testing.T) {
	s, _ := newEmitterStore(t)
	release := make(chan struct{})
	s.SetChangeEmitter(blockingEmitter{release: release})
	t.Cleanup(func() { close(release) })

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- s.CreateAccount(context.Background(), &Account{AccountType: "workbuddy", Token: "t", Enabled: true})
	}()

	select {
	case err := <-writeDone:
		testutil.NoError(t, err, "CreateAccount: %v")
	case <-time.After(2 * time.Second):
		t.Fatal("a blocked emitter stalled the write")
	}

	accounts, err := s.ListAccounts(context.Background())
	testutil.Falsef(t, err != nil || len(accounts) == 0, "the write did not land: %v", err)
}

type blockingEmitter struct{ release chan struct{} }

func (b blockingEmitter) Publish(AccountChange) { <-b.release }

func TestChangeEmitter_CoalescesBurstWithoutGoroutinePerWrite(t *testing.T) {
	s, _ := newEmitterStore(t)
	release := make(chan struct{})
	s.SetChangeEmitter(blockingEmitter{release: release})

	acc := &Account{AccountType: "workbuddy", Token: "t", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount: %v")
	before := runtime.NumGoroutine()
	for i := range 500 {
		acc.Weight = i + 1
		if err := s.UpdateAccount(context.Background(), acc); err != nil {
			close(release)
			t.Fatalf("UpdateAccount %d: %v", i, err)
		}
	}
	after := runtime.NumGoroutine()
	close(release)
	growth := after - before
	testutil.Falsef(t, growth > 10, "account event burst created %d goroutines; want fixed dispatcher", growth)
}

// TestChangeEmitter_IgnoresWritesToAMissingRow documents the delete-then-write
// race: the store's UpdateAccount is a documented no-op for a row that is gone,
// so it must not announce a change either. A subscriber never rebuilds a cache
// entry for an account that does not exist.
func TestChangeEmitter_IgnoresWritesToAMissingRow(t *testing.T) {
	s, _ := newTestRedisStore(t, "missing:")
	emitter := &recordingEmitter{}
	s.SetChangeEmitter(emitter)

	acc := &Account{AccountType: "cline", RefreshToken: "session", Enabled: true}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc), "CreateAccount: %v")
	testutil.NoError(t, s.DeleteAccount(context.Background(), acc.ID), "DeleteAccount: %v")
	afterDelete := len(emitter.waitForChanges(t, 2))
	testutil.Equal(t, afterDelete, 2)

	// The row is gone: the update is a no-op and must stay silent.
	acc.Weight = 5
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc), "UpdateAccount on a missing row: %v")
	time.Sleep(50 * time.Millisecond)
	testutil.Equal(t, len(emitter.all()), afterDelete)
}
