package handler

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"encoding/json"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type spyConnTracker struct {
	mu             sync.Mutex
	counts         map[int64]int64
	acquireCalls   int
	releaseCalls   int
	getCountsCalls int
	fullObserved   chan struct{}
	observeOnce    sync.Once
}

func newSpyConnTracker(counts map[int64]int64) *spyConnTracker {
	cloned := make(map[int64]int64, len(counts))
	maps.Copy(cloned, counts)
	return &spyConnTracker{counts: cloned}
}

func (t *spyConnTracker) Acquire(accountID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.acquireCalls++
	t.counts[accountID]++
}

func (t *spyConnTracker) Release(accountID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseCalls++
	if current := t.counts[accountID]; current > 0 {
		t.counts[accountID] = current - 1
	}
}

func (t *spyConnTracker) GetCount(accountID int64) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[accountID]
}

func (t *spyConnTracker) GetCounts(accountIDs []int64) map[int64]int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.getCountsCalls++
	counts := make(map[int64]int64, len(accountIDs))
	for _, id := range accountIDs {
		counts[id] = t.counts[id]
		if t.fullObserved != nil && counts[id] > 0 {
			t.observeOnce.Do(func() { close(t.fullObserved) })
		}
	}
	return counts
}

type trackerTestUpstream struct {
	err    error
	events []upstream.SSEMessage
}

func (m *trackerTestUpstream) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	if m.err != nil {
		return m.err
	}
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func setupConnTrackerHandlerTest(t *testing.T) (*store.Store, *miniredis.Miniredis) {
	t.Helper()

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{
		RedisAddr:   mini.Addr(),
		RedisDB:     0,
		RedisPrefix: "test:",
	})
	testutil.NoError(t, err, "store.New() error = %v")

	return s, mini
}

func createEnabledTestAccount(t *testing.T, s *store.Store, name, accountType string) *store.Account {
	t.Helper()

	acc := &store.Account{
		Name:        name,
		AccountType: accountType,
		Enabled:     true,
		Weight:      1,
	}
	err := s.CreateAccount(context.Background(), acc)
	testutil.CheckNoError(t, err)
	return acc
}

func TestSelectAccount_UsesHandlerConnTracker(t *testing.T) {
	s, _ := setupConnTrackerHandlerTest(t)

	acc1 := createEnabledTestAccount(t, s, "acc-1", "workbuddy")
	acc2 := createEnabledTestAccount(t, s, "acc-2", "workbuddy")

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	globalTracker := newSpyConnTracker(map[int64]int64{
		acc1.ID: 0,
		acc2.ID: 9,
	})
	lb.SetConnTracker(globalTracker)

	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, lb)
	localTracker := newSpyConnTracker(map[int64]int64{
		acc1.ID: 8,
		acc2.ID: 0,
	})
	h.connTracker = localTracker
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	_, selected, release, err := h.acquireAccountSelection(context.Background(), "workbuddy", true, nil, accountSelectionOptions{})
	defer release()
	testutil.NoError(t, err, "selectAccount() error = %v")
	testutil.False(t, selected == nil, "selectAccount() returned nil account")
	testutil.Equal(t, selected.ID, acc2.ID)
	testutil.NotEqual(t, localTracker.getCountsCalls, 0)
	testutil.Equal(t, globalTracker.getCountsCalls, 0)
}

func TestAcquireReservedAccountSelection_WaitsForShortLease(t *testing.T) {
	s, _ := setupConnTrackerHandlerTest(t)

	acc := createEnabledTestAccount(t, s, "busy-workbuddy", "workbuddy")
	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, lb)
	// The only account is held at its documented single-slot ceiling, so the
	// request has to wait rather than succeed immediately: the old 225ms retry
	// window could return 503 before this release landed.
	acc.MaxConcurrent = 1
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc), "UpdateAccount() error = %v")
	testutil.Equal(t, effectiveAccountConcurrencyLimit(acc), 1)
	tracker := newSpyConnTracker(map[int64]int64{acc.ID: 1})
	tracker.fullObserved = make(chan struct{})
	h.connTracker = tracker
	// A signal under the same lock as the count change proves selection cannot
	// finish before the outstanding lease is released.
	released := make(chan struct{})
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	go func() {
		// Start the delay only after the selector has observed the occupied
		// single slot, rather than while Redis/fixture setup is still running.
		select {
		case <-tracker.fullObserved:
		case <-time.After(3 * time.Second):
		}
		time.Sleep(350 * time.Millisecond)
		tracker.mu.Lock()
		tracker.counts[acc.ID] = 0
		close(released)
		tracker.mu.Unlock()
	}()
	// Also join the helper on failure, so it cannot outlive this test.
	defer func() { <-released }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, chosen, release, trackedID, err := h.acquireReservedAccountSelection(ctx, "workbuddy", true, nil, accountSelectionOptions{ModelID: "deepseek-v4-flash"})
	defer release()
	select {
	case <-tracker.fullObserved:
	default:
		t.Fatal("selector never observed the saturated account")
	}
	select {
	case <-released:
	default:
		t.Fatal("the selection succeeded before the lease was released")
	}
	testutil.NoError(t, err, "acquireReservedAccountSelection() error = %v")
	testutil.Falsef(t, chosen == nil || chosen.ID != acc.ID, "selected account = %#v, want account %d", chosen, acc.ID)
	testutil.Equal(t, trackedID, acc.ID)
	testutil.Equal(t, tracker.GetCount(acc.ID), 1)
	h.releaseTrackedAccount(trackedID)
	release() // The client cache lease is separate from the concurrency slot.
	testutil.Equal(t, tracker.GetCount(acc.ID), 0)
}

func TestAcquireReservedAccountSelection_WaitsForBusyAccountLease(t *testing.T) {
	s, _ := setupConnTrackerHandlerTest(t)

	acc := createEnabledTestAccount(t, s, "busy-workbuddy", "workbuddy")
	// One slot, already taken: the selector must wait for the release below
	// instead of admitting the request against the shared default ceiling.
	acc.MaxConcurrent = 1
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc), "UpdateAccount() error = %v")
	testutil.Equal(t, effectiveAccountConcurrencyLimit(acc), 1)

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, lb)
	tracker := newSpyConnTracker(map[int64]int64{acc.ID: 1})
	tracker.fullObserved = make(chan struct{})
	h.connTracker = tracker
	released := make(chan struct{})
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		return &trackerTestUpstream{}
	})

	go func() {
		// Start the delay only after the selector has observed the occupied
		// single slot, rather than while Redis/fixture setup is still running.
		select {
		case <-tracker.fullObserved:
		case <-time.After(3 * time.Second):
		}
		time.Sleep(350 * time.Millisecond)
		tracker.mu.Lock()
		tracker.counts[acc.ID] = 0
		close(released)
		tracker.mu.Unlock()
	}()
	// Also join the helper on failure, so it cannot outlive this test.
	defer func() { <-released }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, chosen, release, trackedID, err := h.acquireReservedAccountSelection(ctx, "workbuddy", true, nil, accountSelectionOptions{
		ModelID: "claude-opus-5",
	})
	defer release()
	select {
	case <-tracker.fullObserved:
	default:
		t.Fatal("selector never observed the saturated account")
	}
	select {
	case <-released:
	default:
		t.Fatal("the selection succeeded before the lease was released")
	}
	testutil.NoError(t, err, "acquireReservedAccountSelection() error = %v")
	testutil.Falsef(t, chosen == nil || chosen.ID != acc.ID, "selected account = %#v, want account %d", chosen, acc.ID)
	testutil.Equal(t, trackedID, acc.ID)
	testutil.Equal(t, tracker.GetCount(acc.ID), 1)
	h.releaseTrackedAccount(trackedID)
	release() // The client cache lease is separate from the concurrency slot.
	testutil.Equal(t, tracker.GetCount(acc.ID), 0)
}

func TestHandleMessages_AccountSwitchUsesHandlerConnTracker(t *testing.T) {
	s, _ := setupConnTrackerHandlerTest(t)

	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "claude-opus-5"})
	acc1 := createEnabledTestAccount(t, s, "acc-1", "workbuddy")
	acc2 := createEnabledTestAccount(t, s, "acc-2", "workbuddy")
	acc2.MaxConcurrent = 2
	testutil.NoError(t, s.UpdateAccount(context.Background(), acc2), "UpdateAccount(acc-2) error = %v")

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	globalTracker := newSpyConnTracker(nil)
	lb.SetConnTracker(globalTracker)

	cfg := &config.Config{
		DebugEnabled:   false,
		RequestTimeout: 10,
		MaxRetries:     1,
		RetryDelay:     0,
	}
	h := NewWithLoadBalancer(cfg, lb)
	localTracker := newSpyConnTracker(map[int64]int64{
		acc1.ID: 0,
		acc2.ID: 1,
	})
	h.connTracker = localTracker
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		if acc != nil && acc.ID == acc1.ID {
			return &trackerTestUpstream{err: errors.New("HTTP 429 Too Many Requests")}
		}
		if acc != nil && acc.ID == acc2.ID {
			return &trackerTestUpstream{events: []upstream.SSEMessage{
				{Type: "model", Event: map[string]any{"type": "text-start"}},
				{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
				{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
			}}
		}
		return &trackerTestUpstream{}
	})

	payload := map[string]any{
		"model":    "claude-opus-5",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.Equal(t, globalTracker.acquireCalls, 0)
	testutil.Equal(t, globalTracker.releaseCalls, 0)
	testutil.Equal(t, localTracker.acquireCalls, 2)
	testutil.Equal(t, localTracker.releaseCalls, 2)
	testutil.Equal(t, localTracker.GetCount(acc1.ID), 0)
	testutil.Equal(t, localTracker.GetCount(acc2.ID), 1)
}

func TestDefaultAccountConcurrencyLimitIsTen(t *testing.T) {
	t.Parallel()

	// Every provider shares one default. The per-channel values this replaced
	// (WorkBuddy 3, Grok 1, Qoder unlimited) made a channel's capacity depend on
	// which switch arm it happened to fall into.
	for _, accountType := range []string{"grok", "workbuddy", "qoder", "cline"} {
		testutil.Equal(t, effectiveAccountConcurrencyLimit(&store.Account{AccountType: accountType}), 10)
	}
	testutil.Equal(t, effectiveAccountConcurrencyLimit(&store.Account{AccountType: "workbuddy", MaxConcurrent: 7}), 7)
	testutil.Equal(t, effectiveAccountConcurrencyLimit(&store.Account{AccountType: "qoder", MaxConcurrent: 4}), 4)
}

func TestTryAcquireTrackedAccount_DoesNotAdmitRequestPastLimit(t *testing.T) {
	t.Parallel()

	const limit = 10
	tracker := loadbalancer.NewMemoryConnTracker()
	h := &Handler{connTracker: tracker}
	acc := &store.Account{ID: 42, AccountType: "workbuddy"}
	for i := range limit {
		_, ok := h.tryAcquireTrackedAccount(acc)
		testutil.Falsef(t, !ok, "acquire %d unexpectedly rejected", i+1)
	}
	_, ok := h.tryAcquireTrackedAccount(acc)
	testutil.Falsef(t, ok, "request %d was admitted past the %d-slot limit", limit+1, limit)
	testutil.Equal(t, tracker.GetCount(acc.ID), limit)
	for range limit {
		h.releaseTrackedAccount(acc.ID)
	}
	testutil.Equal(t, tracker.GetCount(acc.ID), 0)
}
