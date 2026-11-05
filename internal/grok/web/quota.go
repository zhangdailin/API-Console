package web

import (
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/store"
)

// ApplyWebQuotaInfo persists the independent Web SSO auto/fast windows and
// keeps the legacy aggregate Usage* fields useful to the existing admin UI.
// A partial response is valid: one upstream mode can be temporarily absent.
func ApplyWebQuotaInfo(acc *store.Account, windows map[string]*RateLimitInfo) bool {
	if acc == nil || len(windows) == 0 {
		return false
	}
	now := time.Now().UTC()
	// A mode may be absent from a partial upstream response. Preserve its last
	// still-active observation instead of replacing it with a zero-value window;
	// once its reset passes, absence is allowed to clear it.
	snapshot := acc.GrokWebQuota
	if snapshot.Auto.ResetAt.IsZero() || !now.Before(snapshot.Auto.ResetAt) {
		snapshot.Auto = store.GrokQuotaWindow{}
	}
	if snapshot.Fast.ResetAt.IsZero() || !now.Before(snapshot.Fast.ResetAt) {
		snapshot.Fast = store.GrokQuotaWindow{}
	}
	snapshot.SyncedAt = now
	snapshot.Source = "grok_web_rate_limits"
	if info := windows["auto"]; info != nil {
		snapshot.Auto = quotaWindowFromRateLimitInfo(info)
	}
	if info := windows["fast"]; info != nil {
		snapshot.Fast = quotaWindowFromRateLimitInfo(info)
	}
	changed := acc.GrokWebQuota != snapshot
	acc.GrokWebQuota = snapshot

	// Prefer auto, then fast, for compatibility fields used by older clients.
	var preferred *RateLimitInfo
	if windows["auto"] != nil {
		preferred = windows["auto"]
	} else {
		preferred = windows["fast"]
	}
	if preferred != nil {
		// The aggregate projection carries a limit from ONE mode, so it must not
		// decide the subscription: the same tier shows up as a different number in
		// each mode (auto 150 vs fast 400), and classifying from a single
		// mixed-mode number promotes basic accounts into paid pools. Gate the
		// single-window inference off here and classify from both windows below.
		if applyQuotaInfo(acc, preferred, false) {
			changed = true
		}
	}
	if sub := inferSubscriptionFromWebQuota(windows); sub != "" && acc.Subscription != sub {
		acc.Subscription = sub
		changed = true
	}
	return changed
}

func quotaWindowFromRateLimitInfo(info *RateLimitInfo) store.GrokQuotaWindow {
	if info == nil {
		return store.GrokQuotaWindow{}
	}
	window := store.GrokQuotaWindow{
		Limit:         float64(info.Limit),
		Remaining:     float64(info.Remaining),
		HasLimit:      info.HasLimit,
		HasRemaining:  info.HasRemaining,
		ResetAt:       info.ResetAt,
		WindowSeconds: info.WindowSeconds,
	}
	if info.HasLimit && info.Limit > 0 && info.HasRemaining {
		used := info.Limit - info.Remaining
		if used < 0 {
			used = 0
		}
		window.UsagePercent = float64(used) * 100 / float64(info.Limit)
		window.HasUsage = true
	}
	return window
}

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
// window whose mode is not known (the Console/Build header paths). It must never
// see a Web auto/fast projection: those windows describe the same tiers with
// different numbers, so a mixed-mode number would pick the wrong pool. Web
// snapshots go through inferSubscriptionFromWebQuota instead.
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

// webQuotaTierShapes maps each Web quota mode's window size onto the subscription
// tier the upstream uses for it. The tiers repeat across modes with different
// numbers, which is why the mode has to travel with the limit.
var webQuotaTierShapes = map[string]map[int64]string{
	"auto": {7: "basic", 20: "basic", 25: "lite", 50: "super", 150: "heavy"},
	"fast": {30: "basic", 70: "lite", 140: "super", 400: "heavy"},
}

// Web rate-limit windows also differ in length by plan: Free accounts get a
// 24-hour window, paid plans a 2-hour one. When the counts are a shape the
// table does not know, the length still says whether the account is paid.
const (
	webFreeWindowSeconds = 86400
	webPaidWindowSeconds = 7200
)

// subscriptionRank orders the pools from least to most privileged. Lite is an
// explicit local tier (set by the admin API), never inferred from a window.
var subscriptionRank = map[string]int{"basic": 0, "lite": 1, "super": 2, "heavy": 3}

// inferSubscriptionFromWebQuota classifies a Web account from its independent
// auto/fast windows, taking the LOWEST tier any window reports.
//
// A snapshot can carry contradictory windows (auto 150 while fast 30). Choosing
// the lower tier keeps a basic account out of the paid pools; the reverse
// mistake costs a wasted paid credential and an upstream refusal.
func inferSubscriptionFromWebQuota(windows map[string]*RateLimitInfo) string {
	detected := ""
	// Fixed order so the result does not depend on Go's map iteration.
	for _, mode := range []string{"auto", "fast", "heavy"} {
		info := windows[mode]
		if info == nil || !info.HasLimit || info.Limit <= 0 {
			continue
		}
		candidate := ""
		if mode == "heavy" {
			// The heavy mode exposes a single paid window without tier shapes:
			// any positive limit means the account is on the top tier.
			candidate = "heavy"
		} else if tier, ok := webQuotaTierShapes[mode][info.Limit]; ok {
			candidate = tier
		}
		if candidate == "" && mode != "heavy" {
			switch info.WindowSeconds {
			case webFreeWindowSeconds:
				candidate = "basic"
			case webPaidWindowSeconds:
				// Paid, plan unknown: the lowest paid pool is the safe choice.
				candidate = "lite"
			}
		}
		if candidate == "" {
			continue
		}
		if detected == "" || subscriptionRank[candidate] < subscriptionRank[detected] {
			detected = candidate
		}
	}
	return detected
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
