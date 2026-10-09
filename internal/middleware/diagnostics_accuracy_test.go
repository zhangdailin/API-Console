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
			var v map[string]interface{}
			testutil.NoError(t, json.Unmarshal([]byte(s.Payload), &v), "invalid HTTP summary: %v")
			testutil.Fail(t, v["status"] != float64(200) || v["stream_failed"] != true, v)
		}
	}
	testutil.True(t, found, "diagnostic bundle is missing 6_http_summary.json")
}
