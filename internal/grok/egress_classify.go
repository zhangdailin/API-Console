package grok

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok/grokbase"
)

// 403/429 classification helpers for Build account handling.
// The unified entry point is response-aware (status + body).
//
// A generic 403 is deliberately NOT treated as an account block; only explicit
// "blocked-user"/"user is blocked" language marks the account.

// UpstreamErrorKind categorizes an upstream failure for account/egress handling.
type UpstreamErrorKind int

const (
	UpstreamErrorUnknown UpstreamErrorKind = iota
	UpstreamErrorAccountBlock
	UpstreamErrorGenericForbidden
	UpstreamErrorRateLimited
	UpstreamErrorContentRefusal
	UpstreamErrorUpgradeRequired
)

func (k UpstreamErrorKind) String() string {
	switch k {
	case UpstreamErrorAccountBlock:
		return "account_block"
	case UpstreamErrorGenericForbidden:
		return "generic_forbidden"
	case UpstreamErrorRateLimited:
		return "rate_limited"
	case UpstreamErrorContentRefusal:
		return "content_refusal"
	case UpstreamErrorUpgradeRequired:
		return "upgrade_required"
	default:
		return "unknown"
	}
}

// ClassifyUpstreamResponse classifies an upstream HTTP response.
func ClassifyUpstreamResponse(status int, header http.Header, body []byte) UpstreamErrorKind {
	if status == http.StatusTooManyRequests {
		return UpstreamErrorRateLimited
	}
	switch status {
	case http.StatusUpgradeRequired:
		return UpstreamErrorUpgradeRequired
	case http.StatusForbidden:
		if grokbase.IsAccountBlockBody(body) {
			return UpstreamErrorAccountBlock
		}
		var payload any
		text := string(body)
		if json.Unmarshal(body, &payload) == nil {
			text = strings.Join(grokbase.JSONStrings(payload), " ")
		}
		if apperrors.IsContentPolicyRefusal(text) {
			return UpstreamErrorContentRefusal
		}
		return UpstreamErrorGenericForbidden
	case http.StatusUnauthorized:
	}
	return UpstreamErrorUnknown
}

// ClassifyUpstreamError classifies typed upstream HTTP evidence.
func ClassifyUpstreamError(err error) UpstreamErrorKind {
	if err == nil {
		return UpstreamErrorUnknown
	}
	if typed, ok := errors.AsType[*grokUpstreamError](err); ok {
		return ClassifyUpstreamResponse(typed.status, typed.header, []byte(typed.body))
	}
	return UpstreamErrorUnknown
}
