package grok

import (
	"testing"

	"orchids-api/internal/testutil"
)

// TestPlanDisplayName keeps the names the console used to derive in JavaScript;
// the mapping now lives here so the server is the only place that reads xAI
// plan strings.
func TestPlanDisplayName(t *testing.T) {
	t.Parallel()
	for plan, want := range map[string]string{
		"":                  "",
		"heavy":             "Heavy",
		"SuperGrok Heavy":   "Heavy",
		"x_premium_plus":    "X Premium+",
		"XPremiumPlus":      "X Premium+",
		"x_premium":         "X Premium",
		"xbasic":            "X Basic",
		"supergrok":         "SuperGrok",
		"super":             "Super",
		"pro":               "Super",
		"lite":              "Lite",
		"free":              "Free",
		"basic":             "Basic",
		" enterprise-team ": "enterprise-team",
	} {
		testutil.Equal(t, PlanDisplayName(plan), want)
	}
}
