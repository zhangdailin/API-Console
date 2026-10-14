package accountevents

import (
	"sync"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

type recordingSubscriber struct {
	mu      sync.Mutex
	batches [][]int64
	block   chan struct{}
	panicOn bool
}

func (s *recordingSubscriber) AccountChanges(ids []int64) {
	if s.panicOn {
		panic("subscriber failure")
	}
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]int64(nil), ids...))
}

func (s *recordingSubscriber) all() [][]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]int64(nil), s.batches...)
}

func waitFor(t *testing.T, check func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(message)
}

// TestBus_CoalescesBurstsByAccount is the reason the bus batches: a refresh loop
// touching a dozen fields of one account must produce ONE notification, not one
// cache rebuild per field.
func TestBus_CoalescesBurstsByAccount(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	subscriber := &recordingSubscriber{}
	bus.Subscribe(subscriber)

	for range 25 {
		bus.Publish(Change{AccountID: 7, Kind: KindStatus})
	}

	waitFor(t, func() bool { return len(subscriber.all()) > 0 }, "no batch delivered")
	batches := subscriber.all()
	testutil.Equal(t, len(batches), 1)
	testutil.Equal(t, len(batches[0]), 1)
	testutil.Equal(t, batches[0][0], 7)
}

// TestBus_DeliversEveryAccountOnce keeps the coalescing honest: different
// accounts in one window each appear exactly once.
func TestBus_DeliversEveryAccountOnce(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	subscriber := &recordingSubscriber{}
	bus.Subscribe(subscriber)

	for _, id := range []int64{3, 1, 2, 1, 3} {
		bus.Publish(Change{AccountID: id})
	}

	waitFor(t, func() bool { return len(subscriber.all()) > 0 }, "no batch delivered")
	seen := map[int64]int{}
	for _, batch := range subscriber.all() {
		for _, id := range batch {
			seen[id]++
		}
	}
	for _, id := range []int64{1, 2, 3} {
		testutil.Equal(t, seen[id], 1)
	}
}

// TestBus_NotifiesEverySubscriber covers the three consumers that must all learn
// about a change: pool cache, client cache, refresh scheduler.
func TestBus_NotifiesEverySubscriber(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	pool := &recordingSubscriber{}
	clients := &recordingSubscriber{}
	scheduler := &recordingSubscriber{}
	bus.Subscribe(pool)
	bus.Subscribe(clients)
	bus.Subscribe(scheduler)

	bus.Publish(Change{AccountID: 42, Kind: KindCredential})

	waitFor(t, func() bool {
		return len(pool.all()) == 1 && len(clients.all()) == 1 && len(scheduler.all()) == 1
	}, "not every subscriber was notified")
}

func TestFilteredKickIgnoresStatusAndSchedulerWrites(t *testing.T) {
	kick := NewFilteredKick([]Kind{KindCreated, KindCredential}, store.AccountChangeOriginScheduler)
	kick.AccountChangeBatch([]Change{{AccountID: 1, Kind: KindStatus}})
	kick.AccountChangeBatch([]Change{{AccountID: 1, Kind: KindCredential, Origin: store.AccountChangeOriginScheduler}})

	select {
	case <-kick.Channel():
		t.Fatal("status or scheduler-originated write unexpectedly woke refresh loop")
	case <-time.After(20 * time.Millisecond):
	}

	kick.AccountChangeBatch([]Change{{AccountID: 1, Kind: KindCredential, Origin: "admin"}})
	select {
	case <-kick.Channel():
	case <-time.After(time.Second):
		t.Fatal("admin credential change did not wake refresh loop")
	}
}

// TestBus_SubscriberPanicDoesNotStopDelivery isolates a bad subscriber.
func TestBus_SubscriberPanicDoesNotStopDelivery(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	bad := &recordingSubscriber{panicOn: true}
	good := &recordingSubscriber{}
	bus.Subscribe(bad)
	bus.Subscribe(good)

	bus.Publish(Change{AccountID: 11})

	waitFor(t, func() bool { return len(good.all()) == 1 }, "a panicking subscriber blocked the others")
}

// TestBus_PublishNeverBlocks keeps observability away from the write path: a
// subscribed-but-stuck consumer must not stall a writer.
func TestBus_PublishNeverBlocks(t *testing.T) {
	bus := NewBus()
	defer bus.Close()
	stuck := &recordingSubscriber{block: make(chan struct{})}
	bus.Subscribe(stuck)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range publishBuffer * 3 {
			bus.Publish(Change{AccountID: int64(i%5) + 1})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a stuck subscriber")
	}
}

// TestClassify_StatusAndCredentialChanges pins what a subscriber may assume:
// a credential change is never reported as a plain update.
func TestClassify_StatusAndCredentialChanges(t *testing.T) {
	base := &store.Account{ID: 1, AccountType: "cline", RefreshToken: "session-a", Enabled: true, Weight: 1}

	credentialSwap := *base
	credentialSwap.RefreshToken = "session-b"
	testutil.Equal(t, Classify(base, &credentialSwap), KindCredential)

	disabled := *base
	disabled.Enabled = false
	testutil.Equal(t, Classify(base, &disabled), KindCredential)

	renamed := *base
	renamed.Name = "renamed"
	testutil.Equal(t, Classify(base, &renamed), KindUpdated)

	rejected := *base
	rejected.StatusCode = "401"
	rejected.VerifiedAt = time.Now()
	testutil.Equal(t, Classify(base, &rejected), KindStatus)

	testutil.Equal(t, Classify(nil, base), KindCreated)
	testutil.Equal(t, Classify(base, nil), KindDeleted)
}

// TestClassify_CredentialSignalCoversEveryChannel guards the field list: a
// WorkBuddy refresh-token rotation is a credential change like any other.
func TestClassify_CredentialSignalCoversEveryChannel(t *testing.T) {
	wb := &store.Account{ID: 2, AccountType: "workbuddy", WorkBuddyAccessToken: "a1", WorkBuddyRefreshToken: "r1"}
	rotated := *wb
	rotated.WorkBuddyRefreshToken = "r2"
	testutil.Equal(t, Classify(wb, &rotated), KindCredential)

	grokOAuth := &store.Account{ID: 3, AccountType: "grok", CredentialType: "oauth", OAuthAccessToken: "t1", OAuthRefreshToken: "rr1"}
	refreshed := *grokOAuth
	refreshed.OAuthAccessToken = "t2"
	testutil.Equal(t, Classify(grokOAuth, &refreshed), KindCredential)

	qoder := &store.Account{ID: 4, AccountType: "qoder", QoderAccessToken: "q1"}
	reTokenized := *qoder
	reTokenized.QoderAccessToken = "q2"
	testutil.Equal(t, Classify(qoder, &reTokenized), KindCredential)
}
