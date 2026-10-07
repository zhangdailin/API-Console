package grok

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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
)

func (k UpstreamErrorKind) String() string {
	switch k {
	case UpstreamErrorAccountBlock:
		return "account_block"
	case UpstreamErrorGenericForbidden:
		return "generic_forbidden"
	case UpstreamErrorRateLimited:
		return "rate_limited"
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
	case http.StatusForbidden:
		if IsDefinitiveAccountBlockBody(body) {
			return UpstreamErrorAccountBlock
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
	var typed *grokUpstreamError
	if errors.As(err, &typed) {
		return ClassifyUpstreamResponse(typed.status, typed.header, []byte(typed.body))
	}
	return UpstreamErrorUnknown
}

// IsDefinitiveAccountBlockBody accepts only explicit error code or message
// signals that a Grok account is blocked/suspended.
func IsDefinitiveAccountBlockBody(body []byte) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return IsDefinitiveAccountBlockText(string(body))
	}
	values := collectJSONStrings(payload)
	return IsDefinitiveAccountBlockText(strings.Join(values, " "))
}

// IsDefinitiveAccountBlockText matches explicit account-block language.
func IsDefinitiveAccountBlockText(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "blocked-user") || strings.Contains(value, "user is blocked")
}

// collectJSONStrings recursively gathers string values from a decoded JSON tree
// (objects and arrays) so nested error/message fields are inspected, not just
// the shallow top level.
func collectJSONStrings(value any) []string {
	var out []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for _, child := range typed {
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		case string:
			out = append(out, typed)
		}
	}
	walk(value)
	return out
}
