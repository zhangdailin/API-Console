package api

// Allowance projection: how one account's quota is rendered for the management
// API.
//
// Channels disagree about what their allowance even is — an upstream credit
// window, a rolling token estimate, or nothing but a passive rate-limit header —
// so each channel implements its own projection behind provider.Admin. What they
// share is the shape: fill the same fields and record where the numbers came
// from with provider.QuotaProvenance.

import (
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// projectQuotaFields renders acc's allowance into fields.
//
// limit and current are the account's generic usage slots, already clamped to
// non-negative. Most channels refine them from their own snapshot; observedTokens
// and usageObserved carry the usage this gateway measured itself. An account
// whose channel offers no projection falls back to the rate-limit projection.
func projectQuotaFields(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	if admin := adminFor(acc.AccountType); admin != nil && admin.ProjectQuota != nil {
		admin.ProjectQuota(fields, acc, limit, current, observedTokens, usageObserved)
		return
	}
	projectRateLimitQuota(fields, acc, limit, current, observedTokens, usageObserved)
}

// projectRateLimitQuota is the fallback for channels without a quota projection.
func projectRateLimitQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	fields["quota_limit"] = limit
	remaining := current
	if remaining > limit && limit > 0 {
		remaining = limit
	}
	used := limit - remaining
	if used < 0 {
		used = 0
	}
	fields["quota_used"] = used
	fields["quota_remaining"] = remaining
	// An unregistered channel's numbers come from passive upstream headers, a
	// short-lived throttle window rather than a subscription balance.
	quotaType := "unknown"
	if limit > 0 {
		quotaType = "paid"
	}
	provider.QuotaProvenance(fields, quotaType, "upstreamRateLimit", "observed",
		"来自上游限流响应头，不是套餐余额", limit > 0, false)
}
