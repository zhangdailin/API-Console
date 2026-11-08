package web

import (
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/middleware"
)

var (
	grokSSEEventPrefixBytes = []byte("event: ")
	grokSSEDataPrefixBytes  = []byte("data: ")
	grokSSENewlineBytes     = []byte("\n")
	grokSSEFrameSuffixBytes = []byte("\n\n")
)

// grokErrorCodeForStatus derives a stable machine-readable code from an HTTP
// status so every Grok endpoint answers with the same OpenAI error envelope
// grok2api emits.
func grokErrorCodeForStatus(status int) string {
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

// writeGrokErrorCode writes the shared OpenAI-compatible error object:
//
//	{"error":{"message":…,"type":…,"code":…,"param":null}}
//
// Plain-text bodies (http.Error) cannot be parsed by an OpenAI/Anthropic SDK,
// so every client-visible failure on a Grok endpoint goes through this writer.
func writeGrokErrorCode(w http.ResponseWriter, status int, code, message string) {
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
	writeJSONStatus(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"code":    strings.TrimSpace(code),
			"param":   nil,
		},
	})
}

// writeGrokError writes the shared error object with a code derived from status.
func writeGrokError(w http.ResponseWriter, status int, message string) {
	writeGrokErrorCode(w, status, grokErrorCodeForStatus(status), message)
}

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

// requireMethod writes the standard 405 response and returns false when the
// request method does not match. Handlers use it as:
//
//	if !requireMethod(w, r, http.MethodGet) {
//		return
//	}
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		writeGrokError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

// writeJSON writes v to w as a JSON response with the application/json content
// type.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// maxGrokJSONBodyBytes bounds the request body of every JSON Grok endpoint: an
// unbounded Decode lets one client allocate arbitrary memory in the gateway.
const maxGrokJSONBodyBytes = 32 << 20

// decodeJSONBody decodes the request body into v and writes the standard error
// response on failure: 415 for a non-JSON content type, 413 for an oversized
// body and 400 for malformed JSON.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if r == nil {
		writeGrokError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	if raw := strings.TrimSpace(r.Header.Get("Content-Type")); raw != "" {
		mediaType, _, err := mime.ParseMediaType(raw)
		if err != nil || !strings.EqualFold(mediaType, "application/json") {
			writeGrokErrorCode(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return false
		}
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxGrokJSONBodyBytes)
	}
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			writeGrokErrorCode(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the configured limit")
			return false
		}
		writeGrokError(w, http.StatusBadRequest, "invalid json")
		return false
	}
	return true
}

func requireAPIKeyModel(w http.ResponseWriter, r *http.Request, model string) bool {
	if middleware.APIKeyAllowsModel(r.Context(), model) {
		return true
	}
	// The whole envelope shape is shared with every other Grok error, so a
	// client can parse one object type: message, type, code and param.
	writeGrokErrorCode(w, http.StatusForbidden, "model_not_allowed",
		"API key is not allowed to use model "+strings.TrimSpace(model))
	return false
}

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

// streamResponseHeaders writes the standard SSE headers and returns the
// response flusher (possibly nil).
func streamResponseHeaders(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// Without this a reverse proxy (nginx defaults to proxy_buffering on) holds
	// the frames until the response ends, which silently defeats streaming.
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Connection")
	flusher, _ := w.(http.Flusher)
	return flusher
}

const responseWriteTimeout = 30 * time.Second

// setResponseWriteDeadline bounds downstream backpressure when the writer's
// transport supports deadlines. In-memory/test writers legitimately do not.
func setResponseWriteDeadline(w http.ResponseWriter) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return err
}

// writeSSEBytes sends a raw SSE frame without flushing. Its error result may be
// ignored by legacy non-streaming helpers, while stream loops propagate it.
func writeSSEBytes(w http.ResponseWriter, event string, data []byte) error {
	if err := setResponseWriteDeadline(w); err != nil {
		return err
	}
	var frame []byte
	if event != "" {
		frame = append(frame, grokSSEEventPrefixBytes...)
		frame = append(frame, event...)
		frame = append(frame, grokSSENewlineBytes...)
	}
	frame = append(frame, grokSSEDataPrefixBytes...)
	frame = append(frame, data...)
	frame = append(frame, grokSSEFrameSuffixBytes...)
	return writeAll(w, frame)
}

// writeSSEError sends an OpenAI-style SSE error event (no flush, no [DONE]).
//
// An SSE error is written after the 200 status line is already committed, so the
// HTTP status can no longer describe the outcome. The response writer is told
// about it instead, which is how the operations overview counts a stream that
// died after starting as a failure rather than a success.
func writeSSEError(w http.ResponseWriter, message, errType, code string) {
	middleware.MarkStreamFailure(w)
	requestID := strings.TrimSpace(w.Header().Get(middleware.DiagnosticRequestIDHeader))
	if strings.TrimSpace(errType) == "" {
		errType = "server_error"
	}
	payload := map[string]any{
		// The top-level type is what OpenAI-compatible clients dispatch on; a
		// frame without it looks like an ordinary chunk to them.
		"type": "error",
		"error": map[string]any{
			"message":    apperrors.PublicMessage(message),
			"type":       strings.TrimSpace(errType),
			"code":       strings.TrimSpace(code),
			"request_id": requestID,
		},
	}
	writeSSEBytes(w, "error", encodeJSONBytes(payload))
}

// writeSSE sends an SSE frame and flushes when the writer supports it.
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data []byte) {
	writeSSEBytes(w, event, data)
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSELog sends an SSE frame, mirrors it to the debug logger, and flushes.
func writeSSELog(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, raw []byte) error {
	if err := writeSSEBytes(w, "", raw); err != nil {
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
	return writeSSEBytes(w, "", encodeJSONBytes(payload))
}

func writeChatStreamError(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, msg, code string) {
	_ = writeOpenAIStreamError(w, msg, code)
	_ = writeSSEBytes(w, "", []byte("[DONE]"))
	if logger != nil {
		logger.LogOutputSSE("error", msg)
		logger.LogOutputSSE("", "[DONE]")
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEStreamError sends the named SSE error used by non-Chat protocols.
func writeSSEStreamError(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, msg string) {
	writeSSEError(w, msg, "server_error", "stream_error")
	_ = writeSSEBytes(w, "", []byte("[DONE]"))
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
	writeSSEError(w, message, "server_error", code)
	writeSSE(w, flusher, "", []byte("[DONE]"))
}

// writeGrokNoAccountError lives in pool_error.go: the answer depends on why the
// pool is empty (cooling, rate limited, allowance spent, busy, or truly empty),
// so it shares the classification every other entrance uses.
