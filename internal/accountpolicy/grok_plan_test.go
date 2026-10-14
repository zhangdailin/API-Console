package accountpolicy

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestGrokBuildPlanIsPaid(t *testing.T) {
	t.Parallel()
	for plan, want := range map[string]bool{
		"":               false,
		"unknown":        false,
		" Unknown ":      false,
		"free":           false,
		"basic":          false,
		"supergrok":      true,
		"SuperGrok Pro":  true,
		"heavy":          true,
		"lite":           true,
		"x_basic":        true,
		"xpremium":       true,
		"x_premium_plus": true,
		"team":           true,
		"enterprise":     true,
	} {
		testutil.Equal(t, GrokBuildPlanIsPaid(plan), want)
	}
}
