package web

import (
	"testing"

	"orchids-api/internal/testutil"
)

func TestInferSubscriptionFromRateLimitInfoRequiresKnownShapes(t *testing.T) {
	cases := map[int64]string{
		7: "basic", 20: "basic", 8: "basic", 30: "basic",
		50: "super", 140: "super",
		150: "heavy",
		25:  "lite", 70: "lite", 12: "lite",
		1000: "", 151: "", 149: "", 3: "",
	}
	for limit, want := range cases {
		got := inferSubscriptionFromRateLimitInfo(&RateLimitInfo{Limit: limit, HasLimit: true})
		testutil.Equal(t, got, want)
	}
}
