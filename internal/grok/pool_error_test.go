package grok

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/testutil"
)

// TestWriteGrokAccountUnavailable_ClassifiesThePool pins that the Grok handlers
// answer a pool failure the way every other entrance does. They select their own
// sessions, so before this they answered 503 with the pool's own error text: a
// cooling pool reached the caller as a server fault, and the note
// "no enabled accounts available for channel: grok (…)" — which names internal
// state — travelled with it.
func TestWriteGrokAccountUnavailable_ClassifiesThePool(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "cooling down for the requested model",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are cooling down for the requested model)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "the requested model is cooling down on this channel",
		},
		{
			name:        "pool rate limited",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are rate-limited or cooling down)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "all available accounts for this channel are currently rate-limited",
		},
		{
			name:        "allowance exhausted",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts have exhausted their allowance)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "has exhausted its allowance",
		},
		{
			name:        "all accounts busy",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are at their concurrency limit)"),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "busy with other requests",
		},
		{
			name:        "the pool cannot route the model",
			err:         errors.New("no enabled accounts available for channel: grok (model unavailable-model is not available in the current Grok account pool)"),
			wantStatus:  http.StatusNotFound,
			wantMessage: "is not available on this channel's accounts",
		},
		{
			name:        "a failure that names no capacity cause stays 503 without its detail",
			err:         errors.New("grok cli account token is empty"),
			wantStatus:  http.StatusServiceUnavailable,
			wantMessage: grokResponseAccountUnavailableMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGrokAccountUnavailable(rec, tc.err, "response_account_unavailable", grokResponseAccountUnavailableMessage)

			testutil.Equal(t, rec.Code, tc.wantStatus)
			body := rec.Body.String()
			testutil.MustContain(t, body, tc.wantMessage)
			// The pool's note and the underlying error text are diagnostics: they
			// belong in the log, never in a body a client may show to a user.
			for _, leaked := range []string{"no enabled accounts available", "channel: grok", tc.err.Error()} {
				if leaked == tc.wantMessage {
					continue
				}
				testutil.MustNotContain(t, body, leaked)
			}
		})
	}
}

// TestWriteGrokNoAccountError_ClassifiesTheSameWayAsEveryOtherEntrance covers the
// chat/completions plane: writeGrokNoAccountError used to accept the pool error
// and ignore it, so all eight of its callers answered a cooling pool with 503 and
// a hard-coded sentence. The envelope stays this plane's OpenAI-shaped one; the
// status now follows the cause.
func TestWriteGrokNoAccountError_ClassifiesTheSameWayAsEveryOtherEntrance(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		wantStatus  int
		wantType    string
		wantMessage string
	}{
		{
			name:        "pool rate limited",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts are rate-limited or cooling down)"),
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: "all available accounts for this channel are currently rate-limited",
		},
		{
			name:        "allowance exhausted",
			err:         errors.New("no enabled accounts available for channel: grok (all matching accounts have exhausted their allowance)"),
			wantStatus:  http.StatusTooManyRequests,
			wantType:    "rate_limit_error",
			wantMessage: "has exhausted its allowance",
		},
		{
			name:        "the pool has no accounts at all",
			err:         errors.New("no enabled accounts available for channel: grok"),
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "server_error",
			wantMessage: grokModelAccountUnavailableMessage,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGrokNoAccountError(rec, tc.err)

			testutil.Equal(t, rec.Code, tc.wantStatus)
			body := rec.Body.String()
			testutil.MustContain(t, body, tc.wantMessage)
			testutil.MustContain(t, body, `"type":"`+tc.wantType+`"`)
			testutil.MustNotContain(t, body, "no enabled accounts available")
		})
	}
}

// TestVideoJobFailureMessageKeepsThePoolNoteOutOfTheJob covers the asynchronous
// path: a job is read back with a 200, so whatever is stored is a client-facing
// body. A pool failure is stored as its classified message; every other failure
// keeps its own text, which describes that job rather than the pool.
func TestWriteGrokUpstreamFailure_KeepsProseOutOfTheBody(t *testing.T) {
	upstream := newCLIUpstreamError(403, nil, []byte("{\"error\":{\"code\":\"forbidden\",\"message\":\"Access denied.\"}}"))
	rec := httptest.NewRecorder()
	writeGrokUpstreamFailure(rec, http.StatusForbidden, upstream)

	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)
	body := rec.Body.String()
	testutil.MustContain(t, body, apperrors.PublicMessage(upstream.Error()))
	for _, leaked := range []string{"This page is out of date", "upstream status=", "code\":7"} {
		testutil.MustNotContain(t, body, leaked)
	}

	local := errors.New("audio part is missing a filename")
	rec = httptest.NewRecorder()
	writeGrokUpstreamFailure(rec, http.StatusBadGateway, local)
	testutil.MustContain(t, rec.Body.String(), "audio part is missing a filename")
}
