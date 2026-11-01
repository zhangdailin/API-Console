package grok

import (
	"net/http"

	apperrors "orchids-api/internal/errors"
)

// grokRequestFailure separates terminal request/configuration failures from
// unusable credentials. Messages are stable and never relay upstream bodies.
func grokRequestFailure(err error) (grokPoolAnswer, bool) {
	switch ClassifyUpstreamError(err) {
	case UpstreamErrorContentRefusal:
		return grokRequestFailureByCode("content_policy_violation")
	case UpstreamErrorUpgradeRequired:
		return grokRequestFailureByCode("upstream_upgrade_required")
	default:
		return grokPoolAnswer{}, false
	}
}

func grokRequestFailureByCode(code string) (grokPoolAnswer, bool) {
	switch code {
	case "content_policy_violation":
		return grokPoolAnswer{
			status: http.StatusForbidden, code: "content_policy_violation",
			message: "The upstream declined this request because of its content policy.",
		}, true
	case "upstream_upgrade_required":
		return grokPoolAnswer{
			status: http.StatusServiceUnavailable, code: "upstream_upgrade_required",
			message: apperrors.CategoryMessage("upstream_upgrade_required"),
		}, true
	default:
		return grokPoolAnswer{}, false
	}
}
