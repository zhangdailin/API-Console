package accountpolicy

import (
	"slices"
	"strings"
)

// grokPaidPlanMarkers are the substrings xAI uses in paid Build plan names. The
// Grok channel and the load balancer both decide "paid" from this one list.
var grokPaidPlanMarkers = []string{"super", "pro", "heavy", "lite", "x_basic", "xbasic", "x_premium", "xpremium", "paid", "team", "enterprise"}

// GrokBuildPlanIsPaid reports whether a Grok subscription string names a paid
// Build plan. An empty or "unknown" plan is not evidence of either tier.
func GrokBuildPlanIsPaid(subscription string) bool {
	plan := strings.ToLower(strings.TrimSpace(subscription))
	if plan == "" || plan == "unknown" {
		return false
	}
	return slices.ContainsFunc(grokPaidPlanMarkers, func(paid string) bool { return strings.Contains(plan, paid) })
}
