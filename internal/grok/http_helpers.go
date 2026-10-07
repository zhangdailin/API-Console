package grok

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/httpserver"
)

// The generic half of this file lives in internal/httpserver: the OpenAI error
// envelope, bounded JSON reads and the SSE writers are shared with every other
// inference-facing endpoint, so they must have exactly one implementation. What
// stays here is the Build-specific upstream classifier plus the thin wrappers
// the rest of the package still calls by their short local names.

// writeGrokErrorCode writes the shared OpenAI-compatible error envelope.
func writeGrokErrorCode(w http.ResponseWriter, status int, code, message string) {
	httpserver.WriteErrorCode(w, status, code, message)
}

// writeGrokError writes the shared error object with a code derived from status.
func writeGrokError(w http.ResponseWriter, status int, message string) {
	httpserver.WriteError(w, status, message)
}

// requireMethod rejects every method but the one the endpoint serves.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	return httpserver.RequireMethod(w, r, method)
}

// decodeJSONBody decodes the request body into v under the shared size limit.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	return httpserver.DecodeJSONBody(w, r, v)
}

// readBoundedJSONBody reads a JSON request body under the shared limit.
func readBoundedJSONBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	return httpserver.ReadBoundedJSONBody(w, r)
}

// requireAPIKeyModel rejects a model the caller's key is not allowed to use.
func requireAPIKeyModel(w http.ResponseWriter, r *http.Request, model string) bool {
	return httpserver.RequireAPIKeyModel(w, r, model)
}

// streamResponseHeaders writes the standard SSE headers and returns the flusher.
func streamResponseHeaders(w http.ResponseWriter) http.Flusher {
	return httpserver.StreamResponseHeaders(w)
}

// deadlineResponseWriter refreshes the write deadline on every write.
type deadlineResponseWriter = httpserver.DeadlineResponseWriter

// writeSSEStreamError sends the named SSE error used by non-Chat protocols.
func writeSSEStreamError(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, msg string) {
	httpserver.WriteSSEStreamError(w, flusher, logger, msg)
}

// writeGrokNoAccountError lives in pool_error.go: the answer depends on why the
// pool is empty (cooling, rate limited, allowance spent, busy, or truly empty),
// so it shares the classification every other entrance uses.

// writeGrokUpstreamError maps an upstream failure to what the caller may see.
//
// The upstream response body, the egress node id and the internal "grok cli
// upstream status=…" shape stay in the logs and diagnostics: a caller only ever
// receives a sanitized category message plus a stable code. Credential-class
// failures belong to the account pool the operator owns, so they are answered
// as 503 rather than 401/403, and a retryable failure
// carries the upstream Retry-After back to the client.
func writeGrokUpstreamError(w http.ResponseWriter, err error) {
	if err == nil {
		err = errors.New("upstream request failed")
	}
	if !isUpstreamFailure(err) {
		// A local validation error (missing field, bad multipart part, storage
		// failure) is the caller's own bad request: it keeps its precise message
		// instead of being flattened into an upstream category.
		writeGrokError(w, http.StatusBadRequest, err.Error())
		return
	}
	text := err.Error()
	category := apperrors.ClassifyUpstreamError(text).Category
	status := apperrors.StatusForCategory(category)
	switch category {
	case "auth", "auth_blocked", "configuration":
		// The caller's own key is fine; the account pool needs operator action.
		status = http.StatusServiceUnavailable
	}
	if retryAfter := upstreamRetryAfterSeconds(err); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	writeGrokErrorCode(w, status, category, apperrors.PublicMessage(text))
}

// isUpstreamFailure reports whether err came from an attempt against an upstream
// service. Only those may carry upstream detail, so only those are sanitized;
// everything else is a local error that can be returned as-is.
func isUpstreamFailure(err error) bool {
	if err == nil {
		return false
	}
	var typed *grokUpstreamError
	var synthetic *syntheticCooldownError
	var oauth *cliOAuthError
	if errors.As(err, &typed) || errors.As(err, &synthetic) || errors.As(err, &oauth) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	text := strings.ToLower(err.Error())
	return slices.ContainsFunc([]string{"upstream", "connection reset", "connection refused", "broken pipe", "no such host", "tls handshake"}, func(marker string) bool { return strings.Contains(text, marker) })
}

// upstreamRetryAfterSeconds reads the backoff an upstream asked for. The typed
// upstream error keeps a sanitized header copy, and the rate-limit classifiers
// already understand both the header and the body forms.
func upstreamRetryAfterSeconds(err error) int {
	var typed *grokUpstreamError
	if !errors.As(err, &typed) || typed.header == nil {
		return 0
	}
	if value := parseRetryAfterHeader(typed.header.Get("Retry-After"), time.Now()); value > 0 {
		seconds := int(value.Round(time.Second) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return seconds
	}
	if info := parseRateLimitInfo(typed.header); info != nil {
		if seconds := int(time.Until(info.ResetAt).Round(time.Second) / time.Second); seconds > 0 {
			return seconds
		}
	}
	return 0
}
