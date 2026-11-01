package grok

import (
	"testing"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
)

func TestBuildPlanProjectionUsesEntitlements(t *testing.T) {
	for _, tc := range []struct {
		name, plan, tier string
		billing          store.GrokBillingSnapshot
	}{
		{"free zero weekly", "free", "free", store.GrokBillingSnapshot{Weekly: store.GrokQuotaWindow{HasUsage: true}}},
		{"X Basic is Build free", "x_basic", "free", store.GrokBillingSnapshot{}},
		{"unknown zero weekly", "unknown", "unknown", store.GrokBillingSnapshot{Weekly: store.GrokQuotaWindow{HasUsage: true}}},
		{"unnamed observed weekly", "", "unknown", store.GrokBillingSnapshot{Weekly: store.GrokQuotaWindow{HasUsage: true}}},
		{"unknown nonzero weekly", "unknown", "unknown", store.GrokBillingSnapshot{Weekly: store.GrokQuotaWindow{HasUsage: true, UsagePercent: 80}}},
		{"missing zero values", "unknown", "unknown", store.GrokBillingSnapshot{SyncedAt: time.Now()}},
		{"observed zero profile", "unknown", "free", store.GrokBillingSnapshot{SyncedAt: time.Now(), ProfileKnown: true, Weekly: store.GrokQuotaWindow{HasUsage: true}}},
		{"used profile is not free", "unknown", "unknown", store.GrokBillingSnapshot{SyncedAt: time.Now(), ProfileKnown: true, MonthlyUsed: 1}},
		{"unknown named zero profile", "future_plan", "unknown", store.GrokBillingSnapshot{SyncedAt: time.Now(), ProfileKnown: true}},
		{"positive monthly", "unknown", "paid", store.GrokBillingSnapshot{Monthly: store.GrokQuotaWindow{HasLimit: true, Limit: 10}}},
		{"positive on demand cap", "unknown", "paid", store.GrokBillingSnapshot{OnDemandCap: 10}},
		{"positive on demand spend", "unknown", "paid", store.GrokBillingSnapshot{OnDemandUsed: 1}},
		{"positive prepaid", "unknown", "paid", store.GrokBillingSnapshot{PrepaidBalance: 1}},
		{"free plan with purchased credit", "x_basic", "paid", store.GrokBillingSnapshot{PrepaidBalance: 10}},
		{"paid without quota", "supergrok_lite", "paid", store.GrokBillingSnapshot{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := buildAcc()
			acc.Subscription, acc.GrokBilling = tc.plan, tc.billing
			fields := make(map[string]any)
			projectQuota(fields, acc, 0, 0, 0, false)
			if fields["plan_tier"] != tc.tier || fields["quota_type"] != tc.tier {
				t.Fatalf("plan_tier=%v quota_type=%v, want %s", fields["plan_tier"], fields["quota_type"], tc.tier)
			}
			if accountpolicy.GrokBuildAccountIsPaid(acc) != (tc.tier == "paid") {
				t.Fatal("routing and projected tier disagree")
			}
			if tc.plan == "x_basic" && fields["plan_name"] != "X Basic" {
				t.Fatalf("X Basic name = %v", fields["plan_name"])
			}
		})
	}
}
