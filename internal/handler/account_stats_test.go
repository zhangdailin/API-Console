package handler

import (
	"strconv"
	"testing"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

func TestAccountStatsLimitsQueuedAndInFlightOperations(t *testing.T) {
	h := &Handler{statsPending: make(map[string]accountStatsDelta)}
	for i := range accountStatsQueueLimit {
		if !h.enqueueAccountStats(accountStatsDelta{operationID: strconv.Itoa(i)}) {
			t.Fatalf("queue rejected operation %d below its limit", i)
		}
	}
	inFlight, ok := h.nextAccountStats()
	if !ok || h.enqueueAccountStats(accountStatsDelta{operationID: "overflow"}) {
		t.Fatal("the in-flight operation must still occupy a slot")
	}
	if h.enqueueAccountStats(accountStatsDelta{operationID: inFlight.operationID}) {
		t.Fatal("a duplicate must not displace the operation being persisted")
	}
	if err := h.applyPendingAccountStats(t.Context(), inFlight); err != nil {
		t.Fatal(err)
	}
	if !h.enqueueAccountStats(accountStatsDelta{operationID: "after-expiry"}) {
		t.Fatal("settling an expired operation did not release its slot")
	}
}

func TestAccountStatsFlushSkipsDeletedAccount(t *testing.T) {
	s := newTestRedisStore(t, "account-stats-flush:")
	h := &Handler{loadBalancer: loadbalancer.NewWithCacheTTL(s, time.Minute), statsPending: make(map[string]accountStatsDelta)}
	ctx := t.Context()
	deleted := &store.Account{AccountType: "cline", Enabled: true}
	healthy := &store.Account{AccountType: "cline", Enabled: true}
	for _, row := range []*store.Account{deleted, healthy} {
		if err := s.CreateAccount(ctx, row); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteAccount(ctx, deleted.ID); err != nil {
		t.Fatal(err)
	}
	for _, delta := range []accountStatsDelta{
		{accountID: deleted.ID, operationID: "deleted", usage: 20, count: 1, completedAt: time.Now()},
		{accountID: healthy.ID, operationID: "healthy", usage: 10, count: 1, completedAt: time.Now()},
	} {
		if !h.enqueueAccountStats(delta) {
			t.Fatal("setup could not enqueue statistics")
		}
	}
	// First verify the deleted account directly: iteration over the pending map
	// must not accidentally let the healthy operation hide a permanent error.
	if err := h.applyPendingAccountStats(ctx, h.statsPending["deleted"]); err != nil {
		t.Fatalf("deleted account was treated as retryable: %v", err)
	}
	h.flushPendingAccountStats(time.Second)
	got, err := s.GetAccount(ctx, healthy.ID)
	if err != nil || got.RequestCount != 1 || got.UsageTotal != 10 || len(h.statsPending) != 0 {
		t.Fatalf("the remaining healthy operation did not settle: account=%+v err=%v", got, err)
	}
}
