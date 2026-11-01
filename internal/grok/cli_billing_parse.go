package grok

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

func parseCLIBilling(body []byte) (*CLIBillingInfo, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return nil, fmt.Errorf("decode grok cli billing response: invalid object")
	}
	values := root
	if raw, ok := root["config"]; ok {
		var config map[string]json.RawMessage
		if json.Unmarshal(raw, &config) != nil || config == nil {
			return nil, fmt.Errorf("decode grok cli billing response: invalid config")
		}
		values = config
	}
	info := &CLIBillingInfo{Subscription: cliBillingPlan(values)}
	if info.Subscription == "" {
		info.Subscription = cliBillingPlan(root)
	}
	info.UsagePercent, info.HasUsagePercent = cliBillingNumber(cliBillingFirst(values, "creditUsagePercent", "credit_usage_percent"))
	info.UsagePercent = min(100, max(0, info.UsagePercent))
	limit, hasLimit := cliBillingNumber(cliBillingFirst(values, "monthlyLimit", "monthly_limit"))
	used, hasUsed := cliBillingNumber(cliBillingFirst(values, "used", "totalUsed", "includedUsed"))
	info.MonthlyLimit, info.MonthlyUsed, info.HasMonthly = limit, used, hasLimit || hasUsed
	var hasCap, hasDemandUsed, hasPrepaid bool
	info.OnDemandCap, hasCap = cliBillingNumber(cliBillingFirst(values, "onDemandCap", "on_demand_cap", "maxAmountPerMonth"))
	info.OnDemandUsed, hasDemandUsed = cliBillingNumber(cliBillingFirst(values, "onDemandUsed", "on_demand_used"))
	info.PrepaidBalance, hasPrepaid = cliBillingNumber(cliBillingFirst(values, "prepaidBalance", "prepaid_balance"))
	// Only a complete observed monetary profile can establish zero billing.
	info.ProfileKnown = hasLimit && hasUsed && hasCap && hasDemandUsed && hasPrepaid
	var period map[string]json.RawMessage
	_ = json.Unmarshal(values["currentPeriod"], &period)
	if parsed, err := time.Parse(time.RFC3339, cliBillingString(period, "end")); err == nil {
		info.PeriodEnd = parsed.UTC()
	}
	if !info.HasUsagePercent && info.MonthlyLimit > 0 {
		info.UsagePercent = min(100, max(0, info.MonthlyUsed/info.MonthlyLimit*100))
		info.HasUsagePercent = true
	}
	info.HasBilling = info.HasUsagePercent || !info.PeriodEnd.IsZero() || info.HasMonthly || hasCap || hasDemandUsed || hasPrepaid
	return info, nil
}

func cliBillingFirst(values map[string]json.RawMessage, keys ...string) json.RawMessage {
	for _, key := range keys {
		if raw := values[key]; len(raw) != 0 && string(raw) != "null" {
			return raw
		}
	}
	return nil
}

func cliBillingString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		var value string
		if json.Unmarshal(values[key], &value) == nil && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cliBillingPlan(values map[string]json.RawMessage) string {
	if name := cliBillingString(values, "subscriptionTier", "subscription_tier", "planName", "plan_name", "subscriptionName", "subscription_name", "planCode", "plan_code", "tier"); name != "" {
		return name
	}
	for _, key := range []string{"plan", "subscription", "membership"} {
		if name := cliBillingString(values, key); name != "" {
			return name
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(values[key], &nested) == nil {
			if name := cliBillingString(nested, "name", "displayName", "display_name", "code", "id"); name != "" {
				return name
			}
		}
	}
	return ""
}

func cliBillingNumber(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return 0, false
	}
	var number float64
	if json.Unmarshal(raw, &number) != nil {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			var err error
			number, err = strconv.ParseFloat(strings.TrimSpace(text), 64)
			if err != nil {
				return 0, false
			}
		} else {
			var wrapped map[string]json.RawMessage
			if json.Unmarshal(raw, &wrapped) != nil {
				return 0, false
			}
			return cliBillingNumber(wrapped["val"])
		}
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
		return 0, false
	}
	return number, true
}
