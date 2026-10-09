package grok

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	apperrors "orchids-api/internal/errors"
)

// The fallbacks a client gets when the Grok account pool cannot take a request
// and the failure names no capacity cause. The pool's own note never travels:
// it names internal state ("no enabled accounts available for channel: grok
// (all matching accounts are cooling down for the requested model)"), and the
// same condition is answered by the shared classification below with the status
// it actually implies.
const (
	grokResponseAccountUnavailableMessage = "No Grok account is currently available for this response. Retry later, or check the account pool in Admin UI."
	grokModelAccountUnavailableMessage    = "No upstream account is currently available for this model. Retry later or add capacity."
)

// grokPoolAnswer is one answer to a pool failure: the status and code a client
// acts on, and the text it reads. Each endpoint family formats it with its own
// envelope (writeResponsesAPIError for the responses/voice plane,
// writeGrokErrorCode for the chat/completions plane), so the two families keep
// their shapes while sharing the classification and the wording.
type grokPoolAnswer struct {
	status  int
	code    string
	message string
}

// classifyGrokPoolFailure turns "no account could take this request" into the
// answer the client gets, logging the pool's own note either way.
//
// The Grok handlers select their own Build sessions instead of going through
// the generic session handler, so they used to answer a pool failure with 503 and — in several
// handlers — the pool's error text. A cooling pool, a rate-limited pool and a
// spent allowance are all retryable capacity conditions and are answered 429 by
// the shared classification, the same answer every other entrance gives; a model
// the pool cannot route is a 404. Only the residual cases (a refused credential,
// an empty token, no accounts at all) stay 503, and they lose their internal
// detail: the operator reads it in the log.
func classifyGrokPoolFailure(err error, fallbackCode, fallbackMessage string) grokPoolAnswer {
	if out := apperrors.ClassifyPoolExhaustion(err, ""); !out.Empty() {
		slog.Warn("Grok account pool could not serve the request",
			"error", err, "category", out.Category)
		return grokPoolAnswer{
			status:  apperrors.StatusForCategory(out.Category),
			code:    out.Category,
			message: out.Message,
		}
	}
	slog.Error("No Grok account could serve the request", "error", err, "code", fallbackCode)
	return grokPoolAnswer{
		status:  http.StatusServiceUnavailable,
		code:    fallbackCode,
		message: fallbackMessage,
	}
}

// writeGrokAccountUnavailable answers a request the Grok responses/voice plane
// could not serve.
func writeGrokAccountUnavailable(w http.ResponseWriter, err error, fallbackCode, fallbackMessage string) {
	answer := classifyGrokPoolFailure(err, fallbackCode, fallbackMessage)
	writeResponsesAPIError(w, answer.status, answer.code, answer.message)
}

// writeGrokNoAccountError answers a request the chat/completions plane could not
// serve because the account pool had no usable credential. It is not the
// caller's credential that failed, so the fallback status is 503 with a stable
// code rather than the upstream status (upstream_unavailable) — but a pool that is
// merely cooling down or spent is a
// retryable capacity condition, and that is what the client is told.
func writeGrokNoAccountError(w http.ResponseWriter, err error) {
	answer := classifyGrokPoolFailure(err, "upstream_unavailable", grokModelAccountUnavailableMessage)
	writeGrokErrorCode(w, answer.status, answer.code, answer.message)
}

// grokUpstreamFailureMessage is the text a client may read for a failure that came
// from an upstream service: the shared category sentence. The provider's response
// body, the egress node id and the internal "grok … upstream status=…" shape stay
// in the log — which is how writeGrokUpstreamError has always answered the
// chat/completions plane, and what the Responses/voice plane used to skip by
// writing err.Error().
//
// A local failure (a bad multipart part, a storage error, an interrupted job) is
// the caller's own problem and keeps its precise message, because flattening it
// into "the upstream request failed" would hide the one thing the caller can fix.
func grokUpstreamFailureMessage(err error) string {
	if err == nil {
		return apperrors.PublicMessage("")
	}
	if !isUpstreamFailure(err) {
		return err.Error()
	}
	return apperrors.PublicMessage(err.Error())
}

// writeGrokUpstreamFailure answers an upstream failure on the Responses/voice
// plane. That plane keeps its own envelope and the status the caller already
// computed (a client acts on the status), while the prose goes to the log.
func writeGrokUpstreamFailure(w http.ResponseWriter, status int, err error) {
	var profileErr *buildReasoningProfileError
	if errors.As(err, &profileErr) {
		writeResponsesAPIErrorWithParam(w, http.StatusBadRequest, "invalid_request_error", profileErr.Error(), "reasoning.effort")
		return
	}
	if err != nil {
		slog.Warn("Reporting an upstream failure to the client", "error", err, "status", status)
	}
	// Responses must preserve the same upstream backoff contract as Chat.
	if retryAfter := upstreamRetryAfterSeconds(err); retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	} else {
		var cooldown interface{ RetryAfter() time.Duration }
		if errors.As(err, &cooldown) && cooldown.RetryAfter() > 0 {
			seconds := max(1, int(cooldown.RetryAfter().Round(time.Second)/time.Second))
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
	}
	// A typed upstream credential failure is an operator-owned pool problem, not
	// a rejection of the caller's API key.
	var typed *grokUpstreamError
	if errors.As(err, &typed) && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		status = http.StatusServiceUnavailable
	}
	writeResponsesAPIError(w, status, "upstream_error", grokUpstreamFailureMessage(err))
}
