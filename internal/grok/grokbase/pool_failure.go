package grokbase

import (
	"log/slog"
	"net/http"

	apperrors "orchids-api/internal/errors"
)

// PoolAnswer is one answer to a request the Grok account pool could not
// serve: the status and code a client acts on, and the text it reads. Each
// endpoint family wraps it in its own error envelope.
type PoolAnswer struct {
	Status  int
	Code    string
	Message string
}

// ClassifyPoolFailure turns "no account could take this request" into the
// answer the client gets, logging the pool's own note either way.
//
// A cooling pool, a rate-limited pool and a spent allowance are retryable
// capacity conditions and are answered 429 by the shared classification, the
// same answer every other entrance gives; a model the pool cannot route is a
// 404. Only the residual cases (a refused credential, an empty token, no
// accounts at all) stay 503 with the caller's fallback code and message, and
// they lose their internal detail: the operator reads it in the log.
func ClassifyPoolFailure(err error, fallbackCode, fallbackMessage string) PoolAnswer {
	if out := apperrors.ClassifyPoolExhaustion(err, ""); !out.Empty() {
		slog.Warn("Grok account pool could not serve the request", "error", err, "category", out.Category)
		return PoolAnswer{Status: apperrors.StatusForCategory(out.Category), Code: out.Category, Message: out.Message}
	}
	slog.Error("No Grok account could serve the request", "error", err, "code", fallbackCode)
	return PoolAnswer{Status: http.StatusServiceUnavailable, Code: fallbackCode, Message: fallbackMessage}
}
