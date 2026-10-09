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
	"orchids-api/internal/util"
)

// CLIBillingInfo is the precise weekly Build-credit window exposed by xAI's
// official CLI billing endpoint. The API supplies percentages, not a fixed
// request allowance, so callers must not fabricate one.
type CLIBillingInfo struct {
	UsagePercent    float64
	HasUsagePercent bool
	PeriodEnd       time.Time
	Subscription    string
	MonthlyLimit    float64
	MonthlyUsed     float64
	HasMonthly      bool
}

// FetchBilling reads the current Build weekly-credit window. A missing billing
// response is non-fatal to account authentication and should be represented as
// an unavailable quota rather than an invented plan or allowance.
func (c *CLIClient) FetchBilling(ctx context.Context, acc *store.Account) (*CLIBillingInfo, error) {
	if c == nil || c.oauth == nil || acc == nil {
		return nil, fmt.Errorf("grok cli billing is not configured")
	}
	ApplyCLIOAuthIdentity(acc)
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
	var payload struct {
		SubscriptionTier string `json:"subscriptionTier"`
		Config           struct {
			CreditUsagePercent *float64        `json:"creditUsagePercent"`
			SubscriptionTier   string          `json:"subscriptionTier"`
			MonthlyLimit       json.RawMessage `json:"monthlyLimit"`
			Used               json.RawMessage `json:"used"`
			CurrentPeriod      struct {
				End string `json:"end"`
			} `json:"currentPeriod"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode grok cli billing response: %w", err)
	}
	info := &CLIBillingInfo{Subscription: strings.TrimSpace(util.FirstNonEmpty(payload.SubscriptionTier, payload.Config.SubscriptionTier))}
	if payload.Config.CreditUsagePercent != nil {
		info.HasUsagePercent = true
		info.UsagePercent = *payload.Config.CreditUsagePercent
		if info.UsagePercent < 0 {
			info.UsagePercent = 0
		}
		if info.UsagePercent > 100 {
			info.UsagePercent = 100
		}
	}
	if rawEnd := strings.TrimSpace(payload.Config.CurrentPeriod.End); rawEnd != "" {
		if parsed, err := time.Parse(time.RFC3339, rawEnd); err == nil {
			info.PeriodEnd = parsed.UTC()
		}
	}
	info.MonthlyLimit, info.HasMonthly = cliBillingNumber(payload.Config.MonthlyLimit)
	if used, ok := cliBillingNumber(payload.Config.Used); ok {
		info.MonthlyUsed = used
		info.HasMonthly = info.HasMonthly || ok
	}
	if !info.HasUsagePercent && info.MonthlyLimit > 0 {
		info.UsagePercent = min(100, max(0, info.MonthlyUsed/info.MonthlyLimit*100))
		info.HasUsagePercent = true
	}
	// The billing response does not consistently include the user's paid plan.
	// The official CLI exposes it separately; it is useful account metadata but
	// never a substitute for a numeric quota.
	if tier, tierErr := c.fetchSubscriptionTier(ctx, acc); tierErr == nil && tier != "" {
		info.Subscription = tier
	}
	if !info.HasUsagePercent && info.PeriodEnd.IsZero() && !info.HasMonthly {
		return nil, fmt.Errorf("grok cli billing response contains no weekly quota")
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
	subscription = util.FirstNonEmptyUntrimmed(subscription, "unknown")
	if acc.Subscription != subscription {
		acc.Subscription = subscription
		changed = true
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
	next := store.GrokBillingSnapshot{Weekly: weekly, Monthly: monthly, SyncedAt: time.Now().UTC(), Source: "cli_billing"}
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

func cliBillingNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil {
		return number, true
	}
	var wrapped struct {
		Value float64 `json:"val"`
	}
	if json.Unmarshal(raw, &wrapped) == nil {
		return wrapped.Value, true
	}
	return 0, false
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
	var payload struct {
		SubscriptionTier string `json:"subscriptionTier"`
		User             struct {
			SubscriptionTier string `json:"subscriptionTier"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode grok cli subscription response: %w", err)
	}
	return util.FirstNonEmpty(payload.SubscriptionTier, payload.User.SubscriptionTier), nil
}
