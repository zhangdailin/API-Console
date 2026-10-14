package store

import (
	"context"
	"orchids-api/internal/testutil"
	"sync"
	"testing"
	"time"
)

func TestClaimGrokPaidQuotaProbeIsBoundedAndAtomic(t *testing.T) {
	t.Parallel()
	s, _ := newTestRedisStore(t, "quota-test:")
	ctx := context.Background()
	now := time.Now().UTC()
	acc := &Account{AccountType: "grok", Enabled: true, GrokProvider: "build", Subscription: "super",
		GrokBilling: GrokBillingSnapshot{SyncedAt: now.Add(-time.Hour), Monthly: GrokQuotaWindow{HasLimit: true, Limit: 100, HasRemaining: true, Remaining: 0, ResetAt: now.Add(-time.Second)}}}
	testutil.NoError(t, s.CreateAccount(ctx, acc))

	claims := make(chan bool, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
			testutil.CheckNoError(t, err, "claim: %v")
			claims <- claimed
		})
	}
	wg.Wait()
	close(claims)
	count := 0
	for claimed := range claims {
		if claimed {
			count++
		}
	}
	testutil.Equal(t, count, 1)
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err)
	testutil.Falsef(t, !got.GrokBilling.LastProbeAt.Equal(now) || !got.GrokBilling.NextProbeAt.Equal(now.Add(GrokPaidQuotaProbeInterval)), "probe schedule=%+v", got.GrokBilling)
	claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now.Add(time.Minute))
	testutil.Falsef(t, err != nil || claimed, "probe admitted inside interval: claimed=%v err=%v", claimed, err)
	claimed, err = s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now.Add(GrokPaidQuotaProbeInterval))
	testutil.Falsef(t, err != nil || !claimed, "probe not admitted after interval: claimed=%v err=%v", claimed, err)
}

func TestClaimGrokPaidQuotaProbeWaitsForPeriodEnd(t *testing.T) {
	t.Parallel()
	s, _ := newTestRedisStore(t, "quota-test:")
	ctx := context.Background()
	now := time.Now().UTC()
	acc := &Account{AccountType: "grok", Enabled: true, GrokBilling: GrokBillingSnapshot{SyncedAt: now,
		Weekly: GrokQuotaWindow{HasUsage: true, UsagePercent: 100, ResetAt: now.Add(time.Hour)}}}
	testutil.NoError(t, s.CreateAccount(ctx, acc))
	claimed, err := s.ClaimGrokPaidQuotaProbe(ctx, acc.ID, now)
	testutil.Falsef(t, err != nil || claimed, "claim before period end=%v err=%v", claimed, err)
}
