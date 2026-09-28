package grok

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/store"
)

const (
	basicDefaultQuota float64 = 30
	liteDefaultQuota  float64 = 70
	superDefaultQuota float64 = 140
	heavyDefaultQuota float64 = 400
)

func InferQuotaLimit(acc *store.Account) float64 {
	if acc == nil {
		return basicDefaultQuota
	}
	if acc.UsageLimit > 0 {
		return acc.UsageLimit
	}
	sub := strings.ToLower(strings.TrimSpace(acc.Subscription))
	if strings.Contains(sub, "heavy") {
		return heavyDefaultQuota
	}
	if strings.Contains(sub, "super") || strings.Contains(sub, "pro") {
		return superDefaultQuota
	}
	if strings.Contains(sub, "lite") {
		return liteDefaultQuota
	}
	return basicDefaultQuota
}

// inferSubscriptionFromRateLimitInfo classifies an account from a SINGLE quota
// window whose mode is not known (the Build header paths). It must never
// see a Web auto/fast projection: those windows describe the same tiers with
// different numbers, so a mixed-mode number would pick the wrong pool. Web
// snapshots are Build-only.
func inferSubscriptionFromRateLimitInfo(info *RateLimitInfo) string {
	if info == nil || !info.HasLimit {
		return ""
	}
	// Exact shapes only: an unknown number is left unclassified rather than being
	// rounded up into a paid tier by a ">= heavy" catch-all.
	switch info.Limit {
	case 150:
		return "heavy"
	case 50, 140:
		return "super"
	case 25, 70, 12:
		return "lite"
	case 30, 20, 8, 7:
		return "basic"
	default:
		return ""
	}
}

func ApplyQuotaInfo(acc *store.Account, info *RateLimitInfo) bool {
	return applyQuotaInfo(acc, info, true)
}

// applyQuotaInfo persists one quota window. inferSubscription is false for the
// Web aggregate projection, where the caller classifies from all modes itself.
func applyQuotaInfo(acc *store.Account, info *RateLimitInfo, inferSubscription bool) bool {
	if acc == nil || info == nil {
		return false
	}

	changed := false
	if inferSubscription {
		if sub := inferSubscriptionFromRateLimitInfo(info); sub != "" && acc.Subscription != sub {
			acc.Subscription = sub
			changed = true
		}
	}
	if info.HasRemaining {
		limit := InferQuotaLimit(acc)
		if info.HasLimit && info.Limit > 0 {
			limit = float64(info.Limit)
		}
		remaining := float64(info.Remaining)
		if remaining < 0 {
			remaining = 0
		}
		if limit <= 0 {
			limit = basicDefaultQuota
		}
		if remaining > limit {
			limit = remaining
		}
		if acc.UsageLimit != limit {
			acc.UsageLimit = limit
			changed = true
		}
		if acc.UsageCurrent != remaining {
			acc.UsageCurrent = remaining
			changed = true
		}
	} else if info.HasLimit && info.Limit > 0 && acc.UsageLimit <= 0 {
		acc.UsageLimit = float64(info.Limit)
		changed = true
	}

	if !info.ResetAt.IsZero() && !acc.QuotaResetAt.Equal(info.ResetAt) {
		acc.QuotaResetAt = info.ResetAt
		changed = true
	}
	return changed
}

// ApplyBuildRateLimits persists passive Build response headers separately from
// subscription Billing. These values often describe a minute-scale request or
// token bucket (such as 8300 tokens), never a remaining paid-plan balance.
func ApplyBuildRateLimits(acc *store.Account, headers http.Header) bool {
	if acc == nil || headers == nil {
		return false
	}
	requests := parseBuildRateLimitWindow(headers, "requests")
	tokens := parseBuildRateLimitWindow(headers, "tokens")
	if !requests.HasLimit && !requests.HasRemaining && requests.ResetAt.IsZero() &&
		!tokens.HasLimit && !tokens.HasRemaining && tokens.ResetAt.IsZero() {
		return false
	}
	acc.GrokRateLimits = store.GrokRateLimitSnapshot{
		Requests:   requests,
		Tokens:     tokens,
		ObservedAt: time.Now().UTC(),
	}
	return true
}

func parseBuildRateLimitWindow(headers http.Header, dimension string) store.GrokQuotaWindow {
	limit := firstHeaderValue(headers,
		"x-ratelimit-limit-"+dimension,
		"x-rate-limit-limit-"+dimension,
	)
	remaining := firstHeaderValue(headers,
		"x-ratelimit-remaining-"+dimension,
		"x-rate-limit-remaining-"+dimension,
	)
	reset := firstHeaderValue(headers,
		"x-ratelimit-reset-"+dimension,
		"x-rate-limit-reset-"+dimension,
	)
	window := store.GrokQuotaWindow{}
	if value, ok := parseRateLimitValue(limit); ok {
		window.Limit = float64(value)
		window.HasLimit = true
	}
	if value, ok := parseRateLimitValue(remaining); ok {
		window.Remaining = float64(value)
		window.HasRemaining = true
	}
	window.ResetAt = parseRateLimitReset(reset)
	return window
}

// --- Free (inferred) and estimated allowance -----------------------------------
//
// xAI does not always tell a Build account what it is entitled to: the official
// identity endpoint omits the plan name for Free accounts, and the billing endpoint
// returns no numeric window at all for them. Reporting a bare "额度未知" leaves an
// operator unable to tell "this account is Free" from "this account was never
// synced", while inventing a balance would be a lie. The projection therefore
// carries three extra facts next to every number: WHERE the verdict came from
// (source), HOW strong it is (confidence) and WHETHER the limit is really known
// (limitKnown). An estimated window is only ever produced from those facts, and the
// UI prefixes it with "≈".
const (
	// FreeBuildUsageWindow is the rolling window a Build Free allowance is measured
	// over upstream.
	FreeBuildUsageWindow = 24 * time.Hour
	// EstimatedFreeBuildTokenLimit is the Free window that has been observed from
	// upstream exhaustion payloads. It exists only to give an operator a sense of
	// scale until the upstream reports the real pair; it is always labelled
	// estimated and never replaces official billing data.
	EstimatedFreeBuildTokenLimit int64 = 500_000
)

// Sources of a Free inference, in decreasing strength.
const (
	// FreeProfileSourceBilling: a billing sync succeeded and returned no window and
	// no plan, which is what the upstream does for Free accounts.
	FreeProfileSourceBilling = "billingProfile"
	// FreeProfileSourcePlan: the official identity endpoint reported the Free plan.
	FreeProfileSourcePlan = "subscription"
	// FreeProfileSourceExhaustion: the upstream refused a request for having spent the
	// included free usage. This is the upstream itself saying "this account is Free".
	FreeProfileSourceExhaustion = "upstreamExhaustion"
)

// FreeProfileVerdict is the outcome of the Free inference for one account.
type FreeProfileVerdict struct {
	Inferred bool
	Source   string
}

// BuildPlanIsPaid reports whether a subscription string names a paid Build plan.
// The check exists so a paid account that deliberately exposes no numeric window is
// never reclassified as Free by the estimate below.
func BuildPlanIsPaid(subscription string) bool {
	plan := strings.ToLower(strings.TrimSpace(subscription))
	if plan == "" || plan == "unknown" {
		return false
	}
	for _, paid := range []string{"super", "pro", "heavy", "lite", "x_basic", "xbasic", "x_premium", "xpremium", "paid", "team", "enterprise"} {
		if strings.Contains(plan, paid) {
			return true
		}
	}
	return false
}

// InferFreeProfile decides whether a Build account can be called Free.
//
// An empty or "unknown" plan with no billing sync produces NO verdict: that account
// is genuinely unknown, and saying "Free" about it would be a guess dressed up as a
// fact. Free is inferred only from a successful zero-value billing profile or from
// the upstream's own plan name.
func InferFreeProfile(acc *store.Account) FreeProfileVerdict {
	if acc == nil || ProviderForAccount(acc) != ProviderBuild {
		return FreeProfileVerdict{}
	}
	if BuildPlanIsPaid(acc.Subscription) {
		return FreeProfileVerdict{}
	}
	if !acc.GrokFreeQuota.ConfirmedAt.IsZero() {
		return FreeProfileVerdict{Inferred: true, Source: FreeProfileSourceExhaustion}
	}
	billing := acc.GrokBilling
	if !billing.SyncedAt.IsZero() && !billing.Weekly.HasUsage && !billing.Monthly.HasLimit {
		return FreeProfileVerdict{Inferred: true, Source: FreeProfileSourceBilling}
	}
	switch strings.ToLower(strings.TrimSpace(acc.Subscription)) {
	case "free", "basic":
		return FreeProfileVerdict{Inferred: true, Source: FreeProfileSourcePlan}
	}
	return FreeProfileVerdict{}
}

// freeQuotaExhaustionPattern reads the account's real Free window out of the refusal
// the upstream sends once the included free usage is spent.
var freeQuotaExhaustionPattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

// ApplyFreeQuotaExhaustion records the Free window the upstream just confirmed.
//
// The Free allowance is a rolling window the upstream only reveals when it is already
// exhausted, so this refusal is the single authoritative source for the real
// actual/limit pair. Returning false means the response was not a Free refusal and the
// account must be left untouched. A refusal without a readable pair is still recorded:
// it confirms the account is on Free, which keeps the estimated window honest.
func ApplyFreeQuotaExhaustion(acc *store.Account, body []byte) bool {
	if acc == nil || ProviderForAccount(acc) != ProviderBuild {
		return false
	}
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "subscription:free-usage-exhausted") &&
		!strings.Contains(text, "used all the included free usage") {
		return false
	}
	now := time.Now().UTC()
	previous := acc.GrokFreeQuota
	if !now.After(previous.ConfirmedAt) {
		now = previous.ConfirmedAt.Add(time.Nanosecond)
	}
	snapshot := store.GrokFreeQuotaSnapshot{
		// A refusal without a readable pair still refreshes the window, but it must not
		// erase a limit an earlier refusal did report.
		Used: previous.Used, Limit: previous.Limit, HasLimit: previous.HasLimit,
		ConfirmedAt: now, ResetAt: now.Add(FreeBuildUsageWindow),
	}
	if matches := freeQuotaExhaustionPattern.FindSubmatch(body); len(matches) == 3 {
		used, usedErr := strconv.ParseInt(string(matches[1]), 10, 64)
		limit, limitErr := strconv.ParseInt(string(matches[2]), 10, 64)
		if usedErr == nil && limitErr == nil && limit > 0 {
			snapshot.Used = float64(used)
			snapshot.Limit = float64(limit)
			snapshot.HasLimit = true
		}
	}
	acc.GrokFreeQuota = snapshot
	return true
}
