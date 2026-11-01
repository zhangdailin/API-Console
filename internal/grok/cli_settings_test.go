package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestCLISettingsPlanMatchesObservedBuildResponses(t *testing.T) {
	// Free accounts omit both JWT tier and /user subscriptionTier. The settings
	// label is explicit; a null user tier alone must never be interpreted as Free.
	const sparseBilling = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-11-01T00:00:00Z","end":"2026-11-08T00:00:00Z"},"onDemandCap":{"val":0},"onDemandUsed":{"val":0},"isUnifiedBillingUser":true,"prepaidBalance":{"val":0}}}`
	const percentBilling = `{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","start":"2026-10-27T00:00:00Z","end":"2026-11-03T00:00:00Z"},"creditUsagePercent":64,"isUnifiedBillingUser":true,"prepaidBalance":{"val":0}}}`
	for _, tc := range []struct {
		name, settings, user, billing, claims, wantPlan, wantTier, wantMode, wantSource string
		settingsStatus, billingStatus                                                   int
		wantUserRequest                                                                 bool
	}{
		{"explicit Free with omitted percent", `{"subscription_tier_display":"Free"}`, `{"subscriptionTier":null}`, sparseBilling, `{}`, "Free", "free", "estimated_free", "settings", 200, 200, false},
		{"explicit Free with real percentage", `{"subscription_tier_display":"Free"}`, `{"subscriptionTier":null}`, percentBilling, `{}`, "Free", "free", "weekly_percent", "settings", 200, 200, false},
		{"paid display label", `{"subscription_tier_display":"X Premium"}`, `{"subscriptionTier":"XPremium"}`, percentBilling, `{"tier":3}`, "X Premium", "paid", "weekly_percent", "settings", 200, 200, false},
		{"premium plus display label", `{"subscription_tier_display":"X Premium+"}`, `{"subscriptionTier":null}`, sparseBilling, `{}`, "X Premium+", "paid", "unknown", "settings", 200, 200, false},
		{"X Basic display label", `{"subscriptionTierDisplay":"X Basic"}`, `{"subscriptionTier":null}`, sparseBilling, `{}`, "X Basic", "free", "estimated_free", "settings", 200, 200, false},
		{"settings failure falls back to user", `{}`, `{"subscriptionTier":"supergrok"}`, sparseBilling, `{}`, "supergrok", "paid", "unknown", "user", 503, 200, true},
		{"unknown label falls back to user", `{"subscription_tier_display":"unknown"}`, `{"subscriptionTier":"supergrok_heavy"}`, sparseBilling, `{}`, "supergrok_heavy", "paid", "unknown", "user", 200, 200, true},
		{"missing labels do not imply Free", `{}`, `{"subscriptionTier":null}`, sparseBilling, `{}`, "unknown", "unknown", "unknown", "jwt", 200, 200, true},
		{"invalid settings falls back to JWT", `null`, `{"subscriptionTier":null}`, sparseBilling, `{"tier":6}`, "supergrok_lite", "paid", "unknown", "jwt", 200, 200, true},
		{"settings survives billing outage", `{"subscription_tier_display":"Free"}`, `{"subscriptionTier":null}`, `{}`, `{}`, "Free", "free", "estimated_free", "settings", 200, 503, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var userRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/settings":
					w.WriteHeader(tc.settingsStatus)
					_, _ = io.WriteString(w, tc.settings)
				case "/v1/user":
					userRequests.Add(1)
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
			acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, Subscription: "unknown", OAuthAccessToken: jwtWithClaims(t, tc.claims), OAuthExpiresAt: time.Now().Add(time.Hour)}
			info, err := client.FetchBilling(context.Background(), acc)
			if (err != nil) != (tc.billingStatus != 200) {
				t.Fatalf("billing error = %v", err)
			}
			if info == nil || info.SubscriptionSource != tc.wantSource {
				t.Fatalf("metadata source = %+v", info)
			}
			ApplyCLIBillingInfo(acc, info)
			if acc.Subscription != tc.wantPlan || (userRequests.Load() > 0) != tc.wantUserRequest {
				t.Fatalf("plan=%q userRequests=%d", acc.Subscription, userRequests.Load())
			}
			fields := make(map[string]any)
			projectQuota(fields, acc, 0, 0, 0, false)
			if fields["plan_tier"] != tc.wantTier || fields["quota_mode"] != tc.wantMode {
				t.Fatalf("tier=%v mode=%v, want %s / %s", fields["plan_tier"], fields["quota_mode"], tc.wantTier, tc.wantMode)
			}
			if tc.wantMode == "estimated_free" && (fields["quota_confidence"] != "estimated" || fields["quota_limit_known"] != false) {
				t.Fatal("unreported quota was presented as a confirmed allowance")
			}
			if tc.wantMode == "weekly_percent" && fields["quota_used"] != float64(64) {
				t.Fatal("upstream percentage was changed")
			}
		})
	}
}
