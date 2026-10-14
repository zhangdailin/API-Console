package api

import (
	"testing"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// TestPlanProjectionSharesOneShapeAcrossChannels pins plan_name, plan_tier and
// plan_confidence for every channel, so the console can render one badge from
// one set of fields instead of reading a different field per channel.
func TestPlanProjectionSharesOneShapeAcrossChannels(t *testing.T) {
	t.Parallel()
	synced := time.Now().Add(-time.Minute)
	cases := []struct {
		name                   string
		acc                    *store.Account
		planName, tier, confid string
	}{
		{"cline unsynced", &store.Account{AccountType: "cline"}, "", "unsynced", ""},
		{"cline free", &store.Account{AccountType: "cline", ClinePlan: "free"}, "free", "free", "confirmed"},
		{"cline pass", &store.Account{AccountType: "cline", ClinePlan: "Cline Pass"}, "Cline Pass", "paid", ""},

		{"qoder unsynced", &store.Account{AccountType: "qoder"}, "", "unsynced", ""},
		{"qoder no plan name", &store.Account{AccountType: "qoder", QoderQuota: store.QoderQuotaSnapshot{SyncedAt: synced}}, "", "unknown", ""},
		{"qoder free", &store.Account{AccountType: "qoder", QoderQuota: store.QoderQuotaSnapshot{PlanTier: "Free", SyncedAt: synced}}, "Free", "free", "confirmed"},
		{"qoder pro", &store.Account{AccountType: "qoder", QoderQuota: store.QoderQuotaSnapshot{PlanTier: "Pro", SyncedAt: synced}}, "Pro", "paid", ""},

		{"workbuddy unsynced", &store.Account{AccountType: "workbuddy"}, "", "unsynced", ""},
		{"workbuddy free package", &store.Account{AccountType: "workbuddy", WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{PackageName: "Free Plan Subscription", SyncedAt: synced}}, "Free Plan Subscription", "free", "confirmed"},
		{"workbuddy paid package", &store.Account{AccountType: "workbuddy", WorkBuddyQuota: store.WorkBuddyQuotaSnapshot{PackageName: "Pro Monthly", SyncedAt: synced}}, "Pro Monthly", "paid", ""},

		{"grok unsynced", buildAccount("", store.GrokBillingSnapshot{}), "", "unsynced", ""},
		{"grok unknown", buildAccount("unknown", store.GrokBillingSnapshot{}), "", "unknown", ""},
		{"grok paid plan", buildAccount("supergrok", store.GrokBillingSnapshot{}), "SuperGrok", "paid", ""},
		{"grok billing window proves paid", buildAccount("", store.GrokBillingSnapshot{Monthly: store.GrokQuotaWindow{HasLimit: true, Limit: 10}, SyncedAt: synced}), "", "paid", ""},
		{"grok free from plan name", buildAccount("free", store.GrokBillingSnapshot{}), "", "free", "confirmed"},
		{"grok free from empty billing", buildAccount("", store.GrokBillingSnapshot{SyncedAt: synced}), "", "free", "inferred"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fields := buildQuotaResponseFields(tc.acc)
			testutil.Equal(t, fieldString(t, fields, "plan_name"), tc.planName)
			testutil.Equal(t, fieldString(t, fields, "plan_tier"), tc.tier)
			testutil.Equal(t, fieldString(t, fields, "plan_confidence"), tc.confid)
		})
	}
}

// TestPlanProjectionConfirmsFreeFromExhaustion keeps an upstream refusal for a
// spent free window a stated fact, not an inference.
func TestPlanProjectionConfirmsFreeFromExhaustion(t *testing.T) {
	t.Parallel()
	acc := buildAccount("", store.GrokBillingSnapshot{})
	acc.GrokFreeQuota = store.GrokFreeQuotaSnapshot{ConfirmedAt: time.Now()}
	fields := buildQuotaResponseFields(acc)
	testutil.Equal(t, fieldString(t, fields, "plan_tier"), "free")
	testutil.Equal(t, fieldString(t, fields, "plan_confidence"), "confirmed")
}
