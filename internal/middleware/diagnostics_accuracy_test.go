package middleware

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

type diagnosticJournal struct{ events []audit.Event }

func (l *diagnosticJournal) Log(_ context.Context, e audit.Event) { l.events = append(l.events, e) }

func TestDiagnosticsUniqueIdentityAndUnifiedCompletion(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "identity:")
	journal := &diagnosticJournal{}
	previous, oldDetailed := requestJournal, detailedOutcomeRecorder
	defer func() {
		requestJournal = previous
		detailedOutcomeRecorder = oldDetailed
	}()
	requestJournal = journal
	var durations []int64
	detailedOutcomeRecorder = func(_ context.Context, o opsagg.Outcome) {
		durations = append(durations, o.DurationMS)
		time.Sleep(40 * time.Millisecond)
	}
	observed := ObserveAuditLogger(journal)
	h := TraceMiddleware(Diagnostics(store, func() bool { return true })(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) == "bad-json" {
			http.Error(w, "bad request", 400)
			return
		}
		// An existing handler's normal audit record must not create a duplicate row.
		observed.Log(r.Context(), audit.Event{Action: "grok_request", Kind: audit.KindRequest, Status: "success", Duration: 999, InputTokens: 2})
		w.Write(body)
	}))))
	ids := map[string]bool{}
	for i, body := range []string{"first", "second", "bad-json"} {
		req := httptest.NewRequest("POST", "/grok/v1/responses", strings.NewReader(body))
		req.Header.Set(TraceIDHeader, "client-trace")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		id := rec.Header().Get(DiagnosticRequestIDHeader)
		testutil.Falsef(t, id == "" || id == "client-trace" || ids[id], "nonunique id %q", id)
		ids[id] = true
		testutil.Equal(t, rec.Header().Get(TraceIDHeader), "client-trace")
		b, err := store.Get(context.Background(), id)
		testutil.Fail(t, err != nil || b == nil, b, err)
		testutil.Equal(t, len(journal.events), i+1)
		e := journal.events[i]
		testutil.Equal(t, e.RequestID, id)
		testutil.Equal(t, b.DurationMS, durations[i])
		testutil.Equal(t, e.Duration, b.DurationMS)
		testutil.Fail(t, i == 2 && (e.Status != "error" || e.Metadata["http_status"] != 400), e)
		found := false
		for _, s := range b.Sections {
			if s.Name == "1_http_request.json" {
				found = s.Payload == body
			}
		}
		testutil.True(t, found, "wrong captured request")
	}
	// Verify earlier bundles are still independently addressable after later saves.
	for id := range ids {
		b, _ := store.Get(context.Background(), id)
		testutil.False(t, b == nil, "bundle overwritten")
	}
}

func TestDiagnosticsStreamFailureSummaryMatchesJournal(t *testing.T) {
	mini := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mini.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "failure:")
	journal := &diagnosticJournal{}
	previous := requestJournal
	requestJournal = journal
	defer func() { requestJournal = previous }()
	h := TraceMiddleware(Diagnostics(store, func() bool { return true })(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: hello\n\n"))
		MarkStreamFailure(w)
	}))))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil))
	requestID := rec.Header().Get(DiagnosticRequestIDHeader)
	testutil.NotEqual(t, requestID, "")
	b, err := store.Get(context.Background(), requestID)
	testutil.Falsef(t, err != nil || b == nil, "diagnostic bundle missing: %v", err)
	testutil.Fail(t, len(journal.events) != 1 || journal.events[0].Status != "stream_error", journal.events)
	found := false
	for _, s := range b.Sections {
		if s.Name == "6_http_summary.json" {
			testutil.False(t, found, "duplicate HTTP summary section")
			found = true
			var v map[string]any
			testutil.NoError(t, json.Unmarshal([]byte(s.Payload), &v), "invalid HTTP summary: %v")
			testutil.Fail(t, v["status"] != float64(200) || v["stream_failed"] != true, v)
		}
	}
	testutil.True(t, found, "diagnostic bundle is missing 6_http_summary.json")
}

func TestRequestJournalCarriesUpstreamRoute(t *testing.T) {
	journal := &diagnosticJournal{}
	previous := requestJournal
	requestJournal = journal
	defer func() { requestJournal = previous }()
	h := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.NoteUpstreamCall(r.Context(), "https://cli.example.com/v1/responses?x=1", "grok-build-mapped")
		upstream.NoteServedModel(r.Context(), "grok-build-mapped-0309")
		w.Write([]byte("ok"))
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/grok/v1/chat/completions", strings.NewReader("{}")))
	testutil.Equal(t, len(journal.events), 1)
	meta := journal.events[0].Metadata
	testutil.Equal(t, meta["path"], "/grok/v1/chat/completions")
	testutil.Equal(t, meta[upstream.RouteEndpoint], "/v1/responses")
	testutil.Equal(t, meta[upstream.RouteHost], "cli.example.com")
	testutil.Equal(t, meta[upstream.RouteSentModel], "grok-build-mapped")
	testutil.Equal(t, meta[upstream.RouteServedModel], "grok-build-mapped-0309")
}

// A non-streaming reply that committed 200 early and then failed is journalled
// as an error; the overview must record a failure, not a success.
func TestCommittedTwoHundredThatFailedIsNotASuccess(t *testing.T) {
	journal := &diagnosticJournal{}
	previous, oldDetailed := requestJournal, detailedOutcomeRecorder
	defer func() { requestJournal, detailedOutcomeRecorder = previous, oldDetailed }()
	requestJournal = journal
	var outcomes []opsagg.Outcome
	detailedOutcomeRecorder = func(_ context.Context, o opsagg.Outcome) { outcomes = append(outcomes, o) }
	observed := ObserveAuditLogger(journal)
	h := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(" "))
		observed.Log(r.Context(), audit.Event{Action: "chat_request", Kind: audit.KindRequest, Channel: "cline", Status: "error"})
		w.Write([]byte(`{"error":{"message":"upstream failed"}}`))
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/cline/v1/chat/completions", strings.NewReader("{}")))
	testutil.Equal(t, len(outcomes), 1)
	testutil.False(t, outcomes[0].OK, "a committed 200 that failed was counted as a success")
	testutil.Equal(t, outcomes[0].Status, streamFailureClass)
	testutil.Equal(t, len(journal.events), 1)
	testutil.Equal(t, journal.events[0].Status, "error")
}

// A request refused before any upstream call journalled only its status; the
// reason Codex was given (and the log centre's 失败原因) was missing, so the
// cause had to be inferred from the response length. The journal now carries
// the client-visible error message of a non-streaming error response.
func TestRequestJournalCarriesLocalErrorReason(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		want   string
	}{
		"gateway envelope":   {400, `{"error":{"message":"workbuddy does not support text.verbosity","type":"invalid_request_error"},"type":"error"}`, "workbuddy does not support text.verbosity"},
		"responses envelope": {400, `{"error":{"code":"invalid_request_error","message":"tools[6]: tool type \"custom\" is not supported","param":null,"type":"invalid_request_error"}}`, `tools[6]: tool type "custom" is not supported`},
		"plain text":         {403, "voice relay requires the same origin\n", "voice relay requires the same origin"},
		"success":            {200, `{"error":{"message":"not an error"}}`, ""},
	} {
		t.Run(name, func(t *testing.T) {
			journal := &diagnosticJournal{}
			previous := requestJournal
			requestJournal = journal
			defer func() { requestJournal = previous }()
			h := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				// Written in two pieces, as some writers do.
				_, _ = w.Write([]byte(tc.body[:len(tc.body)/2]))
				_, _ = w.Write([]byte(tc.body[len(tc.body)/2:]))
			})))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/workbuddy/v1/responses", strings.NewReader("{}")))
			testutil.Equal(t, len(journal.events), 1)
			testutil.Equal(t, journal.events[0].Error, tc.want)
		})
	}
}

// Only the head of an error body is kept, and a streamed reply is never
// buffered: the capture costs nothing on the success path.
func TestErrorBodyCaptureIsBounded(t *testing.T) {
	w := NewTracedResponseWriter(httptest.NewRecorder())
	w.WriteHeader(500)
	_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	testutil.Equal(t, len(w.errorHead), errorHeadLimit)
	testutil.Equal(t, len(w.ErrorBodyMessage()), 300)

	sse := httptest.NewRecorder()
	sse.Header().Set("Content-Type", "text/event-stream")
	stream := NewTracedResponseWriter(sse)
	stream.WriteHeader(500)
	_, _ = stream.Write([]byte("data: {\"error\":{\"message\":\"x\"}}\n\n"))
	testutil.Equal(t, len(stream.errorHead), 0)
}
