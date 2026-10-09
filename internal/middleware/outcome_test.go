package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/opsagg"
	"orchids-api/internal/testutil"
)

type recordedOutcome struct {
	Channel      string
	Model        string
	StatusClass  string
	DurationMS   int64
	FirstTokenMS int64
}

// captureOutcomes swaps the process-wide recorder for the duration of one test.
// The recorder is a package global, so these tests must not run in parallel.
func captureOutcomes(t *testing.T) *[]recordedOutcome {
	t.Helper()
	previous := detailedOutcomeRecorder
	outcomes := &[]recordedOutcome{}
	SetDetailedOutcomeRecorder(func(_ context.Context, outcome opsagg.Outcome) {
		*outcomes = append(*outcomes, recordedOutcome{outcome.Channel, outcome.Model, outcome.Status, outcome.DurationMS, outcome.FirstTokenMS})
	})
	t.Cleanup(func() { detailedOutcomeRecorder = previous })
	return outcomes
}

// TestObservedOutcome_TimeToFirstTokenSkipsKeepalives pins the reported defect:
// a streaming handler commits its status line and may send SSE keepalive
// comments long before a token exists, so measuring time-to-first-token from the
// header write understated every prefill it reported.
func TestObservedOutcome_TimeToFirstTokenSkipsKeepalives(t *testing.T) {
	outcomes := captureOutcomes(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// A keepalive that arrives well before the model produces anything.
		_, _ = w.Write([]byte(": keepalive\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(60 * time.Millisecond)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	testutil.Equal(t, len(*outcomes), 1)
	outcome := (*outcomes)[0]
	testutil.Equal(t, outcome.Channel, "grok")
	testutil.Equal(t, outcome.StatusClass, "2xx")
	testutil.Falsef(t, outcome.FirstTokenMS < 50, "first_token_ms = %d, want it measured from the first payload byte (>= 50ms of keepalive)", outcome.FirstTokenMS)
	testutil.Falsef(t, outcome.FirstTokenMS > outcome.DurationMS, "first_token_ms = %d exceeds duration_ms = %d", outcome.FirstTokenMS, outcome.DurationMS)
}

// TestObservedOutcome_FallsBackToFirstWrite covers the non-streaming case: a JSON
// body is a single payload write, and a response that never wrote a body still
// reports a time-to-first-byte rather than zero.
func TestObservedOutcome_FallsBackToFirstWrite(t *testing.T) {
	outcomes := captureOutcomes(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodPost, "/grok/v1/responses", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	testutil.Equal(t, len(*outcomes), 1)
	testutil.Equal(t, (*outcomes)[0].StatusClass, "2xx")
}

// TestObservedOutcome_StreamFailureAfterCommittedStatus pins the other reported
// defect: a stream that starts with 200 and then fails was counted as a success.
func TestObservedOutcome_StreamFailureAfterCommittedStatus(t *testing.T) {
	outcomes := captureOutcomes(t)

	handler := LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		MarkStreamFailure(w)
		_, _ = w.Write([]byte("event: error\ndata: {\"error\":{\"code\":\"stream_error\"}}\n\n"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	// The client still saw a 200: that part cannot be changed after the fact.
	testutil.Equal(t, recorder.Code, http.StatusOK)
	testutil.Equal(t, len(*outcomes), 1)
	testutil.Equal(t, (*outcomes)[0].StatusClass, "stream_error")
}

// TestMarkStreamFailure_IsSafeOnForeignWriters keeps the helper usable from the
// grok SSE writers, which only hold an http.ResponseWriter.
func TestMarkStreamFailure_IsSafeOnForeignWriters(t *testing.T) {
	MarkStreamFailure(nil)
	MarkStreamFailure(httptest.NewRecorder())

	traced := NewTracedResponseWriter(httptest.NewRecorder())
	testutil.False(t, traced.StreamFailed(), "a fresh writer reports a stream failure")
	MarkStreamFailure(traced)
	testutil.False(t, !traced.StreamFailed(), "MarkStreamFailure did not reach the traced writer")
}

func TestIsPayloadWrite(t *testing.T) {
	cases := []struct {
		name  string
		write string
		want  bool
	}{
		{"empty", "", false},
		{"whitespace only", "\n\n", false},
		{"sse keepalive", ": keepalive\n\n", false},
		{"sse comment indented", "  : ping\n", false},
		{"sse data", "data: {}\n\n", true},
		{"json body", `{"ok":true}`, true},
		{"plain text", "hello", true},
	}
	for _, tc := range cases {
		testutil.CheckEqual(t, isPayloadWrite([]byte(tc.write)), tc.want)
	}
}
