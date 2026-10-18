package loadbalancer

import (
	"context"
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestIsAccountAvailable_401RequiresReauth(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "grok", StatusCode: "401", AuthStatus: store.AccountAuthStatusReauthRequired, LastAttempt: time.Now().Add(-24 * time.Hour)}
	testutil.False(t, lb.isAccountAvailable(context.Background(), acc), "reauthRequired account must remain excluded regardless of age")
}

func TestIsAccountAvailable_PaidGrokBillingExhaustion(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "grok", GrokProvider: "build", CredentialType: "oauth", Subscription: "super"}
	acc.GrokBilling.Monthly = store.GrokQuotaWindow{HasLimit: true, Limit: 100, HasRemaining: true, Remaining: 0, ResetAt: time.Now().Add(time.Hour)}
	testutil.False(t, lb.isAccountAvailable(context.Background(), acc), "known exhausted paid Build account must be gated")
	acc.GrokBilling.Monthly.Remaining = 1
	testutil.False(t, !lb.isAccountAvailable(context.Background(), acc), "paid Build account with remaining billing must be available")
}

func TestIsAccountAvailable_Paid402UsesBillingPeriodEnd(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "grok", GrokProvider: "build", CredentialType: "oauth", Subscription: "super", StatusCode: "402", LastAttempt: time.Now().Add(-48 * time.Hour)}
	acc.GrokBilling.Weekly = store.GrokQuotaWindow{HasUsage: true, UsagePercent: 100, ResetAt: time.Now().Add(time.Hour)}
	testutil.False(t, lb.isAccountAvailable(context.Background(), acc), "paid 402 must remain gated until billing period end")
	acc.GrokBilling.Weekly.ResetAt = time.Now().Add(-time.Second)
	// A post-period probe is admitted through the atomic store claim, not by a
	// store-less LoadBalancer: without the claim every concurrent request would
	// hit the exhausted account at once.
	testutil.False(t, lb.isAccountAvailable(context.Background(), acc), "paid 402 must remain gated when no atomic probe store is configured")
}

func TestIsAccountAvailable_429UsesQuotaResetAt(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:           1,
		AccountType:  "workbuddy",
		StatusCode:   "429",
		LastAttempt:  time.Now().Add(-time.Minute),
		QuotaResetAt: time.Now().Add(-time.Second),
	}

	testutil.False(t, !lb.isAccountAvailable(context.Background(), acc), "expected expired quota reset to re-enable account")
	testutil.Equal(t, acc.StatusCode, "")
	testutil.Falsef(t, !acc.QuotaResetAt.IsZero(), "expected quota reset timestamp to be cleared, got %v", acc.QuotaResetAt)
}

func TestIsAccountAvailable_LegacyQoder402ReachesModelFilter(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:          1,
		AccountType: "qoder",
		StatusCode:  "402",
		LastAttempt: time.Now().Add(-5 * time.Minute),
	}

	testutil.False(t, !lb.isAccountAvailable(context.Background(), acc), "expected legacy Qoder 402 account to reach the free-model filter")
	testutil.Equal(t, acc.StatusCode, "402")
}

// TestIsAccountAvailable_WorkBuddyCreditExhaustionReachesModelFilter pins that
// the dedicated spent-package state remains a candidate; the handler's
// model-aware filter then admits only confirmed advertised free models.
func TestIsAccountAvailable_WorkBuddyCreditExhaustionReachesModelFilter(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID: 1, AccountType: "workbuddy", StatusCode: store.AccountStatusWorkBuddyQuotaExhausted,
		StatusMessage: "credits exhausted", LastAttempt: time.Now(), QuotaResetAt: time.Now().Add(48 * time.Hour),
	}
	testutil.False(t, !lb.isAccountAvailable(context.Background(), acc), "expected exhausted WorkBuddy account to reach the free-model filter")
	testutil.Equal(t, acc.StatusCode, store.AccountStatusWorkBuddyQuotaExhausted)
}

// TestIsAccountAvailable_WorkBuddyCreditExhaustionClearsWhenQuotaReturns proves
// that positive quota restores full account capability.
func TestIsAccountAvailable_WorkBuddyCreditExhaustionClearsWhenQuotaReturns(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 1, AccountType: "workbuddy", StatusCode: store.AccountStatusWorkBuddyQuotaExhausted, UsageCurrent: 10}
	testutil.False(t, !lb.isAccountAvailable(context.Background(), acc), "expected account to remain available when quota returns")
	testutil.Equal(t, acc.StatusCode, "")
}

// TestIsAccountAvailable_402KeepsLongCooldownForOtherChannels pins that the
// WorkBuddy release above is channel-scoped: every other provider keeps the
// payment cooldown.
func TestIsAccountAvailable_402KeepsLongCooldownForOtherChannels(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{
		ID:          1,
		AccountType: "other",
		StatusCode:  "402",
		LastAttempt: time.Now().Add(-time.Hour),
	}

	testutil.False(t, lb.isAccountAvailable(context.Background(), acc), "expected non-Qoder 402 account to keep the long cooldown")
}

func TestQoderDailyCountLegacyBillingDateDoesNotBlockRecovery(t *testing.T) {
	lb := &LoadBalancer{connTracker: NewMemoryConnTracker()}
	acc := &store.Account{ID: 85, AccountType: "qoder", StatusCode: "429",
		StatusMessage: "qoder daily request count is exceeded: Billing daily count exceeded",
		LastAttempt:   time.Now().Add(-23 * time.Hour), QuotaResetAt: time.Now().Add(14 * 24 * time.Hour)}
	testutil.False(t, lb.isAccountAvailable(context.Background(), acc))
	acc.LastAttempt = time.Now().Add(-25 * time.Hour)
	testutil.True(t, lb.isAccountAvailable(context.Background(), acc))
	testutil.Equal(t, acc.StatusCode, "")
	testutil.True(t, acc.QuotaResetAt.IsZero())
}
