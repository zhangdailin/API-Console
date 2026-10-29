package errors

import (
	"net/http"
	"strings"
)

// PublicMessage converts an internal/upstream error into a stable, actionable
// message without exposing response bodies, credentials, cookies or provider
// implementation details. The request ID is returned separately by middleware.
func PublicMessage(errText string) string {
	return messageForCategory(ClassifyUpstreamError(errText).Category)
}

// CategoryMessage is the client-visible text for an already-known category.
func CategoryMessage(category string) string { return messageForCategory(category) }

// publicStatusForCategory maps an error category to the HTTP status a client
// should see. It sits beside messageForCategory so the status and the text a
// client receives cannot disagree about what went wrong.
//
// The statuses follow the convention an OpenAI-compatible client already acts on:
// 429 is where it looks for a retryable capacity problem (and for a quota, where
// it stops), 401 for a credential it must replace, 400 for a request the upstream
// rejected on its merits, and 5xx for the gateway's or the upstream's own fault.
//
// Two categories carry the same numeric status for different reasons, which is
// why the table entries are not collapsed:
//
//   - upstream_queue is a throttle the caller can wait out: the upstream put the
//     request in a queue and named a window. It is answered as 429 + Retry-After
//     so an OpenAI-compatible client backs off and comes back, instead of reading
//     503 as a broken gateway.
//   - upstream_unavailable is the upstream's own service being down for this
//     model with no window offered. It is not the caller's rate limit and not
//     this gateway's fault, so it is answered as a temporarily unavailable
//     upstream rather than as a 429 that tells the caller to stop sending.
var publicStatusForCategory = map[string]int{
	"quota_exhausted":      http.StatusTooManyRequests,
	"rate_limit":           http.StatusTooManyRequests,
	"upstream_queue":       http.StatusTooManyRequests,
	"upstream_unavailable": http.StatusServiceUnavailable,
	"auth":                 http.StatusUnauthorized,
	"auth_blocked":         http.StatusUnauthorized,
	"account_restricted":   http.StatusForbidden,
	"client":               http.StatusBadRequest,
	// The caller closed the connection before an answer existed. 499 is the
	// conventional (nginx) status for it; nobody receives the body.
	"client_closed":     499,
	"model_unavailable": http.StatusNotFound,
	"configuration":     http.StatusServiceUnavailable,
	"timeout":           http.StatusGatewayTimeout,
	"network":           http.StatusBadGateway,
	"server":            http.StatusBadGateway,
	"protocol":          http.StatusBadGateway,
	"local_overload":    http.StatusBadGateway,
	// A strict structured-output request the gateway could not satisfy is a fault
	// of the model's answer, not of the caller's request: the caller sent a valid
	// schema and a valid prompt. It is answered as an upstream fault so a client
	// retries or relaxes the schema, rather than as a 400 that tells it its own
	// request was wrong.
	"schema_mismatch": http.StatusBadGateway,
}

func StatusForCategory(category string) int {
	if status, ok := publicStatusForCategory[strings.TrimSpace(category)]; ok {
		return status
	}
	return http.StatusBadGateway
}

// messageForCategory is the single source of truth for the client-visible text
// of each category. An empty category is the caller that had no error text to
// classify, and keeps its shorter wording.
var messageForCategoryTable = map[string]string{
	"configuration":        "This provider is not configured correctly. Contact the gateway administrator.",
	"auth":                 "The upstream account session has expired. Re-authenticate the account and retry.",
	"auth_blocked":         "The upstream account is not allowed to use this feature. Check its plan and permissions.",
	"account_restricted":   "The upstream account is restricted due to unusual activity. Requests are skipped during its restriction window; check the account in the official client or submit an appeal.",
	"quota_exhausted":      "The available upstream accounts have exhausted their quota. Retry after the quota resets or add capacity.",
	"rate_limit":           "The available upstream accounts are rate-limited. Retry after the cooldown.",
	"upstream_queue":       "The upstream is holding this model's requests in a queue and has not admitted this one yet. Retry after the indicated delay.",
	"upstream_unavailable": "The upstream service for this model is temporarily unavailable. Retry after the indicated delay.",
	"model_unavailable":    "The requested model is unavailable for the selected upstream accounts.",
	"client":               "The upstream rejected the request parameters or model. Check the request and model selection.",
	"client_closed":        "The client closed the request before the upstream answered.",
	"timeout":              "The upstream stream timed out while waiting for generated output.",
	"network":              "The gateway lost its connection to the upstream service. Retry later.",
	"server":               "The upstream service is temporarily unavailable. Retry later.",
	"protocol":             "The upstream returned an unsupported stream format. Use the request ID to inspect diagnostics.",
	"local_overload":       "The gateway is temporarily overloaded. Retry later.",
	"schema_mismatch":      "The model's answer did not match the schema this strict request declared. Relax the schema or retry.",
	"canceled":             "The request was canceled.",
}

// The upstream_queue text exists so the caller is told what actually happened. It
// replaces a 503 "the upstream service is temporarily unavailable" that was
// published for a gate the same upstream served fine seconds later.
func messageForCategory(category string) string {
	if message, ok := messageForCategoryTable[strings.TrimSpace(category)]; ok {
		return message
	}
	// An empty category is a caller that had no error text to classify, so it
	// keeps the shorter wording instead of asking for a request ID that does
	// not exist.
	if strings.TrimSpace(category) == "" {
		return "The upstream request failed."
	}
	return "The upstream request failed. Use the request ID to inspect diagnostics."
}
