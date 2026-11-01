package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestRefreshBuildPlanIndependentOfQuota(t *testing.T) {
	for _, tc := range []struct {
		name, user, billing, claims, existing, want string
		userStatus, billingStatus                   int
		wantBillingError, wantQuotaChange           bool
	}{
		{"live beats billing and JWT", `{"subscriptionTier":"x_basic"}`, `{"config":{"subscriptionTier":"heavy","creditUsagePercent":0}}`, `{"tier":5}`, "", "x_basic", 200, 200, false, true},
		{"snake nested user without quota", `{"user":{"subscription_tier":"supergrok_lite"}}`, `{}`, `{"tier":0}`, "free", "supergrok_lite", 200, 200, true, false},
		{"user plan despite billing outage", `{"subscriptionTier":"supergrok"}`, `{}`, `{"tier":0}`, "free", "supergrok", 200, 503, true, false},
		{"billing metadata despite missing quota", `{}`, `{"config":{"plan":{"name":"heavy"}}}`, `{"tier":0}`, "", "heavy", 503, 200, true, false},
		{"JWT fallback despite outages", `{}`, `{}`, `{"tier":4}`, "unknown", "x_premium_plus", 503, 503, true, false},
		{"JWT cannot downgrade known live plan", `{}`, `{}`, `{"tier":0}`, "supergrok", "supergrok", 503, 503, true, false},
		{"missing observations preserve plan", `{}`, `{}`, `{}`, "heavy", "heavy", 503, 503, true, false},
		{"quota only preserves plan", `{}`, `{"config":{"creditUsagePercent":12}}`, `{}`, "heavy", "heavy", 503, 200, false, true},
		{"invalid billing preserves quota and live plan", `{"subscription_tier":"x_basic"}`, `{"config":null}`, `{}`, "heavy", "x_basic", 200, 200, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/user":
					w.WriteHeader(tc.userStatus)
					_, _ = io.WriteString(w, tc.user)
				case "/v1/billing":
					w.WriteHeader(tc.billingStatus)
					_, _ = io.WriteString(w, tc.billing)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			client := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL + "/v1"})
			client.httpClient, client.oauth.httpClient = server.Client(), server.Client()
			before := store.GrokBillingSnapshot{Weekly: store.GrokQuotaWindow{HasUsage: true, UsagePercent: 61}, SyncedAt: time.Now().Add(-time.Hour), Source: "cli_billing"}
			acc := &store.Account{AccountType: "grok", OAuthAccessToken: jwtWithClaims(t, tc.claims), OAuthExpiresAt: time.Now().Add(time.Hour), Subscription: tc.existing, GrokBilling: before, UsageCurrent: 7, UsageLimit: 10}
			result := RefreshBuildAccount(context.Background(), client, acc, BuildRefreshOptions{Billing: true})
			if (result.BillingErr != nil) != tc.wantBillingError {
				t.Fatalf("billing error = %v", result.BillingErr)
			}
			if acc.Subscription != tc.want {
				t.Errorf("subscription = %q, want %q", acc.Subscription, tc.want)
			}
			if (acc.GrokBilling != before) != tc.wantQuotaChange {
				t.Errorf("quota replaced = %v, want %v", acc.GrokBilling != before, tc.wantQuotaChange)
			}
			if !tc.wantQuotaChange && (acc.UsageCurrent != 7 || acc.UsageLimit != 10) {
				t.Fatal("metadata-only refresh changed usage counters")
			}
		})
	}
}

func TestParseCLIBillingMonetaryEvidence(t *testing.T) {
	shadowed, err := parseCLIBilling([]byte(`{"monthlyLimit":99,"config":{"creditUsagePercent":7}}`))
	if err != nil || shadowed.HasMonthly || shadowed.MonthlyLimit != 0 {
		t.Fatalf("outer billing fields leaked into config: %+v, %v", shadowed, err)
	}
	info, err := parseCLIBilling([]byte(`{"config":{"monthly_limit":{"val":"200"},"used":"50","on_demand_cap":"10","on_demand_used":{"val":3},"prepaid_balance":7}}`))
	if err != nil || !info.ProfileKnown || !info.HasBilling || info.MonthlyLimit != 200 || info.MonthlyUsed != 50 || info.UsagePercent != 25 || info.OnDemandCap != 10 || info.OnDemandUsed != 3 || info.PrepaidBalance != 7 {
		t.Fatalf("parse monetary profile: info=%+v err=%v", info, err)
	}
	for _, raw := range []string{`{}`, `{"val":null}`, `"NaN"`, `"Inf"`, `-1`, `true`} {
		if _, ok := cliBillingNumber([]byte(raw)); ok {
			t.Errorf("invalid amount %s accepted", raw)
		}
	}
	for _, raw := range []string{`{}`, `{"creditUsagePercent":0}`, `{"monthlyLimit":0}`} {
		info, err := parseCLIBilling([]byte(raw))
		if err != nil || info.ProfileKnown {
			t.Errorf("incomplete profile became known: %+v, %v", info, err)
		}
	}
}
