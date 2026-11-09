package web

import (
	"errors"
	"net/http"
	"strings"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
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

// grokPoolAnswer is the shared pool-failure answer (grokbase.PoolAnswer).
type grokPoolAnswer = grokbase.PoolAnswer

// classifyGrokPoolFailure is grokbase.ClassifyPoolFailure.
func classifyGrokPoolFailure(err error, fallbackCode, fallbackMessage string) grokPoolAnswer {
	return grokbase.ClassifyPoolFailure(err, fallbackCode, fallbackMessage)
}

// writeGrokNoAccountError answers a request the chat/completions plane could not
// serve because the account pool had no usable credential. It is not the
// caller's credential that failed, so the fallback status is 503 with a stable
// code rather than the upstream status (grok2api answers the same situation with
// upstream_unavailable) — but a pool that is merely cooling down or spent is a
// retryable capacity condition, and that is what the client is told.
func writeGrokNoAccountError(w http.ResponseWriter, err error) {
	if writeMediaUnavailable(w, err) {
		return
	}
	answer := classifyGrokPoolFailure(err, "upstream_unavailable", grokModelAccountUnavailableMessage)
	httpserver.WriteErrorCode(w, answer.Status, answer.Code, answer.Message)
}

// writeMediaUnavailable answers a media request no account has allowance for
// with 429 and the reason, instead of the generic pool sentence.
func writeMediaUnavailable(w http.ResponseWriter, err error) bool {
	var media *mediaUnavailableError
	if !errors.As(err, &media) {
		return false
	}
	httpserver.WriteErrorCode(w, http.StatusTooManyRequests, "media_quota_exhausted", media.message)
	return true
}

// carriesPoolReason reports whether a selector error names why the pool is empty,
// as opposed to the bare "no enabled accounts available for channel: X".
func carriesPoolReason(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "matching accounts") || strings.Contains(text, "account pool")
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
