package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/store"
)

// CLIBillingInfo is the precise weekly Build-credit window exposed by xAI's
// official CLI billing endpoint. The API supplies percentages, not a fixed
// request allowance, so callers must not fabricate one.
type CLIBillingInfo struct {
	UsagePercent       float64
	HasUsagePercent    bool
	PeriodEnd          time.Time
	Subscription       string
	SubscriptionSource string
	HasBilling         bool
	ProfileKnown       bool
	OnDemandCap        float64
	OnDemandUsed       float64
	PrepaidBalance     float64
	MonthlyLimit       float64
	MonthlyUsed        float64
	HasMonthly         bool
}

// FetchBilling reads the current Build weekly-credit window. A missing billing
// response is non-fatal to account authentication. Partial metadata is returned
// alongside a billing error and must still be applied without replacing quota.
func (c *CLIClient) FetchBilling(ctx context.Context, acc *store.Account) (*CLIBillingInfo, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli billing is not configured")
	}
	ApplyCLIOAuthIdentity(acc)
	// Read plan metadata independently so a missing billing window cannot erase it.
	tierTimeout := 5 * time.Second
	if deadline, ok := ctx.Deadline(); ok {
		tierTimeout = min(tierTimeout, time.Until(deadline)/2)
	}
	tierCtx, cancel := context.WithTimeout(ctx, tierTimeout)
	tier, _ := c.fetchSubscriptionTier(tierCtx, acc)
	cancel()
	info, billingErr := c.fetchBillingSnapshot(ctx, acc)
	if info == nil {
		info = &CLIBillingInfo{}
	}
	switch {
	case tier != "":
		info.Subscription, info.SubscriptionSource = tier, "user"
	case info.Subscription != "":
		info.SubscriptionSource = "billing"
	default:
		info.Subscription = cliSubscriptionTierFromJWT(acc.OAuthAccessToken)
		info.SubscriptionSource = "jwt"
	}
	return info, billingErr
}

func (c *CLIClient) fetchBillingSnapshot(ctx context.Context, acc *store.Account) (*CLIBillingInfo, error) {
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/billing?format=credits", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, newCLIUpstreamError(resp.StatusCode, resp.Header, body)
	}
	info, err := parseCLIBilling(body)
	if err != nil {
		return nil, err
	}
	if !info.HasBilling {
		return info, fmt.Errorf("grok cli billing response contains no quota profile")
	}
	return info, nil
}

// ApplyCLIBillingInfo records only official Billing state. Response headers
// use an independent short-lived throttling snapshot, so they can never turn
// into a fake subscription balance (for example 8300 / 8300 after one chat).
func ApplyCLIBillingInfo(acc *store.Account, info *CLIBillingInfo) bool {
	if acc == nil || info == nil {
		return false
	}
	changed := false
	// The plan comes from the official CLI identity endpoint. It is metadata,
	// not an inferred allowance: some paid accounts intentionally do not expose
	// a numeric Build-credit window.
	subscription := strings.TrimSpace(info.Subscription)
	existing := strings.TrimSpace(acc.Subscription)
	if subscription != "" && (info.SubscriptionSource != "jwt" || existing == "" || strings.EqualFold(existing, "unknown")) && acc.Subscription != subscription {
		acc.Subscription = subscription
		changed = true
	}
	if !info.HasBilling && !info.HasUsagePercent && info.PeriodEnd.IsZero() && !info.HasMonthly {
		return changed
	}
	weekly := store.GrokQuotaWindow{}
	usagePercent, hasUsagePercent := info.UsagePercent, info.HasUsagePercent
	if !hasUsagePercent && info.MonthlyLimit > 0 {
		usagePercent = min(100, max(0, info.MonthlyUsed/info.MonthlyLimit*100))
		hasUsagePercent = true
	}
	if hasUsagePercent {
		weekly.HasUsage = true
		weekly.UsagePercent = usagePercent
	}
	if !info.PeriodEnd.IsZero() {
		weekly.ResetAt = info.PeriodEnd
	}
	monthly := store.GrokQuotaWindow{}
	if info.HasMonthly {
		monthly.HasLimit = info.MonthlyLimit > 0
		monthly.Limit = info.MonthlyLimit
		monthly.HasRemaining = info.MonthlyLimit > 0
		monthly.Remaining = max(0, info.MonthlyLimit-info.MonthlyUsed)
	}
	next := store.GrokBillingSnapshot{Weekly: weekly, Monthly: monthly, SyncedAt: time.Now().UTC(), Source: "cli_billing", ProfileKnown: info.ProfileKnown, MonthlyUsed: info.MonthlyUsed, OnDemandCap: info.OnDemandCap, OnDemandUsed: info.OnDemandUsed, PrepaidBalance: info.PrepaidBalance}
	if acc.GrokBilling != next {
		acc.GrokBilling = next
		changed = true
	}
	// Clear legacy flat counters written by pre-provider code. The current
	// account UI reads GrokBilling and GrokRateLimits instead.
	if acc.UsageLimit != 0 || acc.UsageCurrent != 0 {
		acc.UsageLimit = 0
		acc.UsageCurrent = 0
		changed = true
	}
	return changed
}

func (c *CLIClient) fetchSubscriptionTier(ctx context.Context, acc *store.Account) (string, error) {
	resp, err := c.request(ctx, acc, http.MethodGet, c.baseURL()+"/user?include=subscription", nil, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, cliOAuthMaxBodyBytes))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", newCLIUpstreamError(resp.StatusCode, resp.Header, body)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode grok cli subscription response: %w", err)
	}
	if tier := cliBillingString(payload, "subscriptionTier", "subscription_tier"); tier != "" {
		return tier, nil
	}
	var user map[string]json.RawMessage
	_ = json.Unmarshal(payload["user"], &user)
	return cliBillingString(user, "subscriptionTier", "subscription_tier"), nil
}
