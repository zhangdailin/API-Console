package errors

import (
	"net/http"
	"testing"

	"orchids-api/internal/testutil"
)

func TestWorkBuddyRestrictionClassificationIsExact(t *testing.T) {
	for _, message := range []string{
		"workbuddy API error: status=403, code=11140, message=request illegal",
		"wrapped: workbuddy API error: status=403, code=11140",
	} {
		testutil.True(t, IsWorkBuddyRestricted(message))
		testutil.Equal(t, ClassifyUpstreamError(message).Category, "account_restricted")
		testutil.Equal(t, ClassifyPoolExhaustion(nil, message).Category, "account_restricted")
	}
	for _, message := range []string{
		"workbuddy API error: status=403, code=111400",
		"workbuddy API error: status=403, code=11128",
		"workbuddy API error: status=500, code=11140",
		"qoder API error: status=403, code=11140",
		"workbuddy stream request failed: HTTP 403",
	} {
		testutil.False(t, IsWorkBuddyRestricted(message))
	}
	testutil.Equal(t, StatusForCategory("account_restricted"), http.StatusForbidden)
}
