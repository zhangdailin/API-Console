package loadbalancer

import (
	"context"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestGetNextAccountExcludingByChannelWithTracker_AllRateLimitedReturnsHelpfulError(t *testing.T) {
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "WorkBuddy1", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now},
			{ID: 2, Name: "WorkBuddy2", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "workbuddy", nil)
	testutil.False(t, err == nil, "expected rate-limited selector error, got nil")
	testutil.MustContain(t, err.Error(), "all matching accounts are rate-limited or cooling down")
}

// TestGetNextAccountExcludingByChannelWithTracker_AllAllowanceParkedNamesTheAllowance
// pins the second reason an empty pool has: every account is parked by an
// exhausted allowance with its reset time still ahead. The caller has to be able
// to tell that apart from a rate limit, because the action differs — credits and
// capacity, rather than waiting out a cooldown.
func TestGetNextAccountExcludingByChannelWithTracker_AllAllowanceParkedNamesTheAllowance(t *testing.T) {
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "WB1", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 2, Name: "WB2", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "workbuddy", nil)
	testutil.False(t, err == nil, "expected an allowance-parked selector error, got nil")
	testutil.MustContain(t, err.Error(), "have exhausted their allowance")
}

// TestGetNextAccountExcludingByChannelWithTrackerFilter_ModelFilterEmptiesThePool
// pins the reason that produced the WorkBuddy outage this was written for: the
// channel has accounts, but every one of them is cooling down for the model the
// request asked for. The bare "no enabled accounts available for channel" made
// that read like a channel with no accounts at all.
func TestGetNextAccountExcludingByChannelWithTrackerFilter_ModelFilterEmptiesThePool(t *testing.T) {
	now := time.Now()
	tracker := NewMemoryConnTracker()
	lb := &LoadBalancer{
		connTracker:    tracker,
		cachedAccounts: []*store.Account{{ID: 1, Name: "WB1", AccountType: "workbuddy", Enabled: true}},
		cacheExpires:   now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTrackerFilter(context.Background(), nil, "workbuddy", tracker, func(*store.Account) error {
		// The per-model cooldown filter: every candidate is withheld for this model.
		return ErrModelThrottled
	})
	testutil.False(t, err == nil, "expected a model-filtered selector error, got nil")
	testutil.MustContain(t, err.Error(), "cooling down for the requested model")
}

// TestGetNextAccountExcludingByChannelWithTracker_MixedPoolNamesTheSplit is the
// regression test for the 2026-09-21 WorkBuddy outage.
//
// The pool held three accounts cooling down from a 429 and four parked for a
// spent allowance at the same time. Both group-only rules require *every*
// account to share one reason, so neither matched, the selector fell through to
// the bare "no enabled accounts available for channel: workbuddy", and that
// sentence classifies to no capacity cause at all — the caller was answered with
// a 503 "server fault" instead of a retryable 429, and the operator could not
// tell rate limits from spent credits.
func TestGetNextAccountExcludingByChannelWithTracker_MixedPoolNamesTheSplit(t *testing.T) {
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: NewMemoryConnTracker(),
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "WB1", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now, RateLimitFailures: 1},
			{ID: 2, Name: "WB2", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now, RateLimitFailures: 1},
			{ID: 3, Name: "WB3", AccountType: "workbuddy", Enabled: true, StatusCode: "429", LastAttempt: now, RateLimitFailures: 1},
			{ID: 4, Name: "WB4", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 5, Name: "WB5", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 6, Name: "WB6", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
			{ID: 7, Name: "WB7", AccountType: "workbuddy", Enabled: true, StatusCode: "402", StatusMessage: "credits exhausted", LastAttempt: now, QuotaResetAt: now.Add(48 * time.Hour)},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "workbuddy", nil)
	testutil.False(t, err == nil, "expected a mixed-pool selector error, got nil")
	message := err.Error()
	testutil.MustContain(t, message, "rate-limited or cooling down")
	testutil.MustContainAll(t, message, "3 rate-limited", "4 parked for a spent allowance")
	// The phrase "exhausted their allowance" would make the shared pool rule
	// classify a recoverable mixed pool as a permanent quota verdict.
	testutil.MustNotContain(t, message, "exhausted their allowance")
	// And it must not degrade to the bare sentence that answered 503.
	testutil.Falsef(t, strings.HasSuffix(message, "channel: workbuddy"), "mixed pool fell back to the unexplained selector sentence: %v", err)
}

func TestGetNextAccountExcludingByChannelWithTracker_RejectsSingleAccountAtLimit(t *testing.T) {
	tracker := NewMemoryConnTracker()
	tracker.Acquire(1)
	now := time.Now()
	lb := &LoadBalancer{
		connTracker: tracker,
		cachedAccounts: []*store.Account{
			{ID: 1, Name: "Grok1", AccountType: "grok", Enabled: true, MaxConcurrent: 1},
		},
		cacheExpires: now.Add(time.Minute),
	}

	_, err := lb.GetNextAccountExcludingByChannelWithTracker(context.Background(), nil, "grok", tracker)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "concurrency limit"), "expected concurrency limit error, got %v", err)
}
