package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/middleware"
	"orchids-api/internal/util"
)

// writeGrokUpstreamError maps an upstream failure to what the caller may see.
//
// The upstream response body, the egress node id and the internal "grok cli
// upstream status=…" shape stay in the logs and diagnostics: a caller only ever
// receives a sanitized category message plus a stable code. Credential-class
// failures belong to the account pool the operator owns, so they are answered
// as 503 rather than 401/403 (grok2api does the same), and a retryable failure
// carries the upstream Retry-After back to the client.
func writeGrokUpstreamError(w http.ResponseWriter, err error) {
	if err == nil {
		err = errors.New("upstream request failed")
	}
	if writeMediaUnavailable(w, err) {
		return
	}
	if !isUpstreamFailure(err) {
		// A local validation error (missing field, bad multipart part, storage
		// failure) is the caller's own bad request: it keeps its precise message
		// instead of being flattened into an upstream category.
		httpserver.WriteError(w, http.StatusBadRequest, err.Error())
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
	// The client only gets the category sentence, so the cause has to be in
	// the log (the Build plane logs the same line). Upstream errors carry
	// sanitized headers and a bounded body, never a credential.
	slog.Warn("Reporting an upstream failure to the client", "error", err, "status", status, "category", category)
	if retryAfter := upstreamRetryAfterSeconds(err); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	httpserver.WriteErrorCode(w, status, category, apperrors.PublicMessage(text))
}

// upstreamStatusMarker is the stamp this codebase puts on an error that really
// came from an upstream HTTP response (see grokUpstreamError.Error).
//
// Matching the whole marker rather than a bare "status=" is what keeps a local
// error out of the upstream branch. An error that merely mentions a status —
// "job status=404 not found in store", "account status=402 parked" — is a local
// condition, and classifying it as upstream both answers 4xx as 5xx and replaces
// the operator's message with a generic sentence.
const upstreamStatusMarker = "upstream status="

// isUpstreamFailure reports whether err came from an attempt against an upstream
// service. Only those may carry upstream detail, so only those are sanitized;
// everything else is a local error that can be returned as-is.
func isUpstreamFailure(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[*grokUpstreamError](err); ok {
		return true
	}
	if strings.Contains(strings.ToLower(err.Error()), upstreamStatusMarker) && parseUpstreamStatus(err) > 0 {
		return true
	}
	// Both transports in this channel render the same failure: the CLI/Build
	// client prefixes "grok cli upstream status=…", the chat relay "grok upstream
	// status=…". A bare "status=" without either prefix is a local condition and
	// must stay local.
	if text := strings.ToLower(err.Error()); parseUpstreamStatus(err) > 0 &&
		(strings.HasPrefix(strings.TrimSpace(text), "grok cli upstream") || strings.HasPrefix(strings.TrimSpace(text), "grok upstream")) {
		return true
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return true
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"upstream", "connection reset", "connection refused", "broken pipe", "no such host", "tls handshake"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// upstreamRetryAfterSeconds reads the backoff an upstream asked for. The typed
// upstream error keeps a sanitized header copy, and the rate-limit classifiers
// already understand both the header and the body forms.
func upstreamRetryAfterSeconds(err error) int {
	var typed *grokUpstreamError
	if !errors.As(err, &typed) {
		return 0
	}
	return grokbase.RetryAfterSeconds(typed.header)
}

// maxGrokJSONBodyBytes bounds the request body of every JSON Grok endpoint: an
// unbounded Decode lets one client allocate arbitrary memory in the gateway.
const maxGrokJSONBodyBytes = 32 << 20

// requireGrokStore writes the standard 503 response and returns false when the
// handler has no account store. Admin handlers use it as:
//
//	if !requireGrokStore(w, h) {
//		return
//	}
func requireGrokStore(w http.ResponseWriter, h *Handler) bool {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		http.Error(w, "store not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// writeSSE sends an SSE frame and flushes when the writer supports it.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data []byte) {
	httpserver.WriteSSEBytes(w, event, data)
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSELog sends an SSE frame, mirrors it to the debug logger, and flushes.
func writeSSELog(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, raw []byte) error {
	if err := httpserver.WriteSSEBytes(w, "", raw); err != nil {
		return err
	}
	if logger != nil {
		logger.LogOutputSSE("", string(raw))
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

// writeOpenAIStreamError emits the data-only error envelope expected by Chat
// Completions clients. Responses and Anthropic keep their named error events.
func writeOpenAIStreamError(w http.ResponseWriter, message, code string) error {
	middleware.MarkStreamFailure(w)
	payload := map[string]any{"error": map[string]any{
		"message": apperrors.PublicMessage(message), "type": "api_error", "code": strings.TrimSpace(code),
	}}
	return httpserver.WriteSSEBytes(w, "", util.EncodeJSONBytes(payload))
}

func writeChatStreamError(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, msg, code string) {
	_ = writeOpenAIStreamError(w, msg, code)
	_ = httpserver.WriteSSEBytes(w, "", []byte("[DONE]"))
	if logger != nil {
		logger.LogOutputSSE("error", msg)
		logger.LogOutputSSE("", "[DONE]")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSECodedError sends a typed SSE error frame followed by [DONE] and flushes.
// Use this when the error code is not the generic stream_error.
func writeSSECodedError(w http.ResponseWriter, flusher http.Flusher, message, code string) {
	httpserver.WriteSSEError(w, message, "server_error", code)
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

// writeGrokNoAccountError lives in pool_error.go: the answer depends on why the
// pool is empty (cooling, rate limited, allowance spent, busy, or truly empty),
// so it shares the classification every other entrance uses.
