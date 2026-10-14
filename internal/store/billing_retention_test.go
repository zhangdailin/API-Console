package store

import (
	"context"
	"fmt"
	"orchids-api/internal/billingledger"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestBillingReceiptsExpireWithoutResettingUsage(t *testing.T) {
	s, mini := newTestRedisStore(t, "retention:")
	key := createBillingKey(t, s, 0)
	ctx := context.Background()
	for i := range 200 {
		testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, billingledger.EventID(time.Now(), fmt.Sprint(i)), 2))
	}
	receipts := 0
	for _, name := range mini.Keys() {
		if strings.Contains(name, ":receipt:") {
			receipts++
			testutil.True(t, mini.TTL(name) > 0)
		}
	}
	testutil.Equal(t, receipts, 200)
	mini.FastForward(billingledger.Retention + time.Hour)
	for _, name := range mini.Keys() {
		testutil.False(t, strings.Contains(name, ":receipt:"))
	}
	got, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.BillingUsedUSDTicks, int64(400))
	testutil.Error(t, s.SettleApiKeyBilling(ctx, key.ID, billingledger.EventID(time.Now().Add(-billingledger.Retention-time.Hour), "0"), 2))
	got, err = s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.BillingUsedUSDTicks, int64(400))
}
func TestBillingResetPreservesReplayProtection(t *testing.T) {
	s, _ := newTestRedisStore(t, "reset-replay:")
	key := createBillingKey(t, s, 1000)
	ctx := context.Background()
	event := billingledger.EventID(time.Now(), "request")
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, event, 100))
	testutil.NoError(t, s.ResetApiKeyBilling(ctx, key.ID))
	testutil.NoError(t, s.SettleApiKeyBilling(ctx, key.ID, event, 100))
	got, err := s.GetApiKeyByID(ctx, key.ID)
	testutil.NoError(t, err)
	testutil.Equal(t, got.BillingUsedUSDTicks, int64(0))
}
