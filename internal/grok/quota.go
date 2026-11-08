package grok

import (
	"net/http"
	"orchids-api/internal/grok/ratelimit"
	"regexp"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

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
	limit := ratelimit.FirstHeader(headers,
		"x-ratelimit-limit-"+dimension,
		"x-rate-limit-limit-"+dimension,
	)
	remaining := ratelimit.FirstHeader(headers,
		"x-ratelimit-remaining-"+dimension,
		"x-rate-limit-remaining-"+dimension,
	)
	reset := ratelimit.FirstHeader(headers,
		"x-ratelimit-reset-"+dimension,
		"x-rate-limit-reset-"+dimension,
	)
	window := store.GrokQuotaWindow{}
	if value, ok := ratelimit.ParseValue(limit); ok {
		window.Limit = float64(value)
		window.HasLimit = true
	}
	if value, ok := ratelimit.ParseValue(remaining); ok {
		window.Remaining = float64(value)
		window.HasRemaining = true
	}
	window.ResetAt = ratelimit.ParseReset(reset)
	return window
}

// --- Free (inferred) and estimated allowance -----------------------------------
//
// xAI does not always tell a Build account what it is entitled to: the official
// identity endpoint omits the plan name for Free accounts, and the billing endpoint
// returns no numeric window at all for them. Reporting a bare "quota unknown" leaves an
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

// PlanDisplayName renders a Build subscription string as the plan name an
// operator recognises. An unrecognised string is returned trimmed as-is.
func PlanDisplayName(subscription string) string {
	raw := strings.TrimSpace(subscription)
	plan := accountpolicy.NormalizeGrokPlan(raw)
	switch {
	case plan == "":
		return ""
	case strings.Contains(plan, "heavy"):
		return "Heavy"
	case strings.Contains(plan, "xpremiumplus"):
		return "X Premium+"
	case strings.Contains(plan, "xpremium"):
		return "X Premium"
	case strings.Contains(plan, "xbasic"):
		return "X Basic"
	case strings.Contains(plan, "lite"):
		return "Lite"
	case strings.Contains(plan, "supergrok"):
		return "SuperGrok"
	case strings.Contains(plan, "super"), strings.Contains(plan, "pro"):
		return "Super"
	case strings.Contains(plan, "free"):
		return "Free"
	case strings.Contains(plan, "basic"):
		return "Basic"
	default:
		return raw
	}
}

// projectPlan publishes the account's tier in the shared plan_* shape. A paid
// plan name or a positive monetary entitlement proves paid; a Free verdict is confirmed when
// the upstream stated it (plan endpoint or an exhaustion refusal) and inferred
// when it rests on an empty billing profile.
func projectPlan(fields map[string]any, acc *store.Account) {
	subscription := strings.TrimSpace(acc.Subscription)
	if accountpolicy.GrokBuildAccountIsPaid(acc) {
		provider.PlanProjection(fields, PlanDisplayName(subscription), provider.PlanTierPaid, "")
		return
	}
	if verdict := InferFreeProfile(acc); verdict.Inferred {
		confidence := provider.PlanConfidenceConfirmed
		if verdict.Source == FreeProfileSourceBilling {
			confidence = provider.PlanConfidenceInferred
		}
		name := ""
		if accountpolicy.GrokBuildPlanIsFree(subscription) {
			name = PlanDisplayName(subscription)
		}
		provider.PlanProjection(fields, name, provider.PlanTierFree, confidence)
		return
	}
	switch {
	case subscription == "":
		tier := provider.PlanTierUnsynced
		if !acc.GrokBilling.SyncedAt.IsZero() || acc.GrokBilling.Weekly.HasUsage {
			tier = provider.PlanTierUnknown
		}
		provider.PlanProjection(fields, "", tier, "")
	case strings.EqualFold(subscription, "unknown"):
		provider.PlanProjection(fields, "", provider.PlanTierUnknown, "")
	default:
		provider.PlanProjection(fields, PlanDisplayName(subscription), provider.PlanTierUnknown, "")
	}
}

// InferFreeProfile decides whether a Build account can be called Free.
//
// An empty or "unknown" plan with no billing sync produces NO verdict: that account
// is genuinely unknown, and saying "Free" about it would be a guess dressed up as a
// fact. Free is inferred only from a successful zero-value billing profile or from
// the upstream's own plan name.
func InferFreeProfile(acc *store.Account) FreeProfileVerdict {
	if acc == nil || !accountpolicy.IsGrokAccount(acc) || accountpolicy.GrokBuildAccountIsPaid(acc) {
		return FreeProfileVerdict{}
	}
	if accountpolicy.GrokBuildPlanIsFree(acc.Subscription) {
		return FreeProfileVerdict{Inferred: true, Source: FreeProfileSourcePlan}
	}
	if !acc.GrokFreeQuota.ConfirmedAt.IsZero() {
		return FreeProfileVerdict{Inferred: true, Source: FreeProfileSourceExhaustion}
	}
	billing := acc.GrokBilling
	if !cliPlanNameAvailable(acc.Subscription) && billing.HasZeroProfile() {
		return FreeProfileVerdict{Inferred: true, Source: FreeProfileSourceBilling}
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
	if acc == nil || !accountpolicy.IsGrokAccount(acc) {
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
