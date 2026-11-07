package grok

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok/grokbase"
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
	grokFreeUsageExhaustedMessage         = "The selected upstream account has exhausted its included free usage for this model. Retry later or use another model."
)

// grokPoolAnswer is the shared pool-failure answer (grokbase.PoolAnswer).
type grokPoolAnswer = grokbase.PoolAnswer

// classifyGrokPoolFailure is grokbase.ClassifyPoolFailure.
func classifyGrokPoolFailure(err error, fallbackCode, fallbackMessage string) grokPoolAnswer {
	return grokbase.ClassifyPoolFailure(err, fallbackCode, fallbackMessage)
}

// writeGrokAccountUnavailable answers a request the Grok responses/voice plane
// could not serve.
func writeGrokAccountUnavailable(w http.ResponseWriter, err error, fallbackCode, fallbackMessage string) {
	answer := classifyGrokPoolFailure(err, fallbackCode, fallbackMessage)
	writeResponsesAPIError(w, answer.Status, answer.Code, answer.Message)
}

// writeGrokNoAccountError answers a request the chat/completions plane could not
// serve because the account pool had no usable credential. It is not the
// caller's credential that failed, so the fallback status is 503 with a stable
// code rather than the upstream status (upstream_unavailable) — but a pool that is
// merely cooling down or spent is a
// retryable capacity condition, and that is what the client is told.
func writeGrokNoAccountError(w http.ResponseWriter, err error) {
	answer := classifyGrokPoolFailure(err, "upstream_unavailable", grokModelAccountUnavailableMessage)
	writeGrokErrorCode(w, answer.Status, answer.Code, answer.Message)
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
	if answer, ok := grokRequestFailure(err); ok {
		return answer.Message
	}
	if err == nil {
		return apperrors.PublicMessage("")
	}
	if isFreeUsageExhaustedError(err) {
		return grokFreeUsageExhaustedMessage
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
	if answer, ok := grokRequestFailure(err); ok {
		writeResponsesAPIError(w, answer.Status, answer.Code, answer.Message)
		return
	}
	if profileErr, ok := errors.AsType[*buildReasoningProfileError](err); ok {
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
	code := "upstream_error"
	if isFreeUsageExhaustedError(err) {
		status, code = http.StatusTooManyRequests, "free_usage_exhausted"
	}
	writeResponsesAPIError(w, status, code, grokUpstreamFailureMessage(err))
}
