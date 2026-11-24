package web

import (
	"errors"
	"log/slog"
	"net/http"
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
	grokbase.WritePublicUpstreamError(w, err, upstreamRetryAfterSeconds(err), func(err error, status int, category string) {
		slog.Warn("Reporting an upstream failure to the client", "error", err, "status", status, "category", category)
	})
}

func isUpstreamFailure(err error) bool {
	_, typed := errors.AsType[*grokUpstreamError](err)
	return typed || grokbase.IsUpstreamTransportFailure(err)
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

// MaxChatJSONBodyBytes is also enforced on internally bridged chat requests.
const MaxChatJSONBodyBytes = maxGrokJSONBodyBytes

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
	logger.LogOutputSSEBytes("", raw)
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
