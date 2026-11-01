package accountpolicy

import (
	"strings"

	"orchids-api/internal/store"
)

// NormalizeGrokPlan folds case, separators and "+" so every spelling of one
// Build plan name compares equal ("X Premium+" == "x_premium_plus").
func NormalizeGrokPlan(subscription string) string {
	return strings.NewReplacer(" ", "", "_", "", "-", "", "+", "plus").Replace(strings.ToLower(strings.TrimSpace(subscription)))
}

// GrokBuildPlanIsFree names Build entitlements, independently of an X subscription.
func GrokBuildPlanIsFree(subscription string) bool {
	switch NormalizeGrokPlan(subscription) {
	case "free", "grokfree", "freetier", "basic", "grokbasic", "xbasic":
		return true
	}
	return false
}

// GrokBuildPlanIsPaid reports whether a Grok subscription string names a paid
// Build plan. An empty or "unknown" plan is not evidence of either tier.
func GrokBuildPlanIsPaid(subscription string) bool {
	plan := NormalizeGrokPlan(subscription)
	switch plan {
	case "super", "pro", "grokpro", "heavy", "lite", "xpremium", "xpremiumplus", "paid", "team", "enterprise", "apikey":
		return true
	}
	return strings.HasPrefix(plan, "supergrok") || strings.HasPrefix(plan, "enterprise") || strings.HasPrefix(plan, "team")
}

// GrokBuildAccountIsPaid shares plan and monetary evidence with account routing.
// Positive purchased credit is an entitlement even on an otherwise free plan.
func GrokBuildAccountIsPaid(acc *store.Account) bool {
	if !IsGrokAccount(acc) {
		return false
	}
	return GrokBuildPlanIsPaid(acc.Subscription) || acc.GrokBilling.HasPaidEntitlement()
}
