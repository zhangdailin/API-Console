// Package httpserver holds the HTTP plumbing every inference-facing endpoint
// shares: the OpenAI-compatible error envelope, bounded JSON body reads, and
// the SSE writers. It is deliberately free of protocol and provider knowledge,
// so both the Grok/Build native paths and the OpenAI Responses bridge can
// answer with byte-identical envelopes.
package httpserver

import (
	"net/http"
	"strings"

	"orchids-api/internal/util"
)

// ErrorCodeForStatus derives a stable machine-readable code from an HTTP status
// so every endpoint answers with the same OpenAI error envelope.
func ErrorCodeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusForbidden:
		return "permission_denied"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusMethodNotAllowed:
		return "method_not_allowed"
	case http.StatusConflict:
		return "conflict"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusUnsupportedMediaType:
		return "unsupported_media_type"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	case http.StatusGatewayTimeout:
		return "timeout"
	}
	if status >= 500 {
		return "server_error"
	}
	return "invalid_request"
}

// WriteErrorCode writes the shared OpenAI-compatible error object:
//
//	{"error":{"message":…,"type":…,"code":…,"param":null}}
//
// Plain-text bodies (http.Error) cannot be parsed by an OpenAI/Anthropic SDK,
// so every client-visible failure goes through this writer.
//
// A status below 400 is raised to 502: a caller that reached this writer has
// failed, and answering 200 with an error body would hide the failure from
// every client that only looks at the status line.
func WriteErrorCode(w http.ResponseWriter, status int, code, message string) {
	if status < 400 {
		status = http.StatusBadGateway
	}
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	errorType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		errorType = "authentication_error"
	case status == http.StatusTooManyRequests:
		errorType = "rate_limit_error"
	case status >= 500:
		errorType = "server_error"
	}
	util.WriteJSONStatus(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"code":    strings.TrimSpace(code),
			"param":   nil,
		},
	})
}

// WriteError writes the shared error object with a code derived from status.
func WriteError(w http.ResponseWriter, status int, message string) {
	WriteErrorCode(w, status, ErrorCodeForStatus(status), message)
}
