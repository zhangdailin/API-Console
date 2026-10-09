package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/testutil"
)

func TestDiagnosticWriterPreservesLongResponse(t *testing.T) {
	_, capture := debug.WithCapture(context.Background(), "long-response")
	defer capture.Close()
	recorder := httptest.NewRecorder()
	writer := &diagnosticWriter{TracedResponseWriter: NewTracedResponseWriter(recorder), capture: capture}
	payload := strings.Repeat("中文", 40000) + "FINAL_SENTINEL"
	writer.Write([]byte(payload))
	b := capture.Bundle()
	testutil.False(t, b.Truncated || len(b.Sections) != 1 || b.Sections[0].Payload != payload || recorder.Body.String() != payload, "response lost content")
}

func TestDiagnosticsStreamingAndExactlyOneOutcome(t *testing.T) {
	old := detailedOutcomeRecorder
	defer func() { detailedOutcomeRecorder = old }()
	var outcomes []opsagg.Outcome
	SetDetailedOutcomeRecorder(func(_ context.Context, o opsagg.Outcome) { outcomes = append(outcomes, o) })
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "test:")
	log := ObserveAuditLogger(audit.NewNopLogger())
	original := `{"model":"test","messages":[{"content":"hello"}]}`
	handler := TraceMiddleware(Diagnostics(store, func() bool { return true })(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		testutil.CheckEqual(t, string(raw), original)
		r = r.WithContext(WithRequestModel(r.Context(), "test"))
		log.Log(r.Context(), audit.Event{Action: "grok_upstream_attempt", AccountID: 1, Status: "error"})
		log.Log(r.Context(), audit.Event{Action: "grok_upstream_attempt", AccountID: 2, Status: "success"})
		log.Log(r.Context(), audit.Event{Action: "grok_request", Status: "ok", InputTokens: 11, OutputTokens: 22})
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: hello\n\n")
		w.(http.Flusher).Flush()
		MarkStreamFailure(w)
	}))))
	req := httptest.NewRequest("POST", "/grok/v1/responses", strings.NewReader(original))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	testutil.False(t, recorder.Body.String() != "data: hello\n\n" || !recorder.Flushed, "streaming changed")
	testutil.Equal(t, len(outcomes), 1)
	o := outcomes[0]
	testutil.Falsef(t, o.OK || o.Status != "stream_error" || o.InputTokens != 11 || o.OutputTokens != 22 || o.AttemptFailures != 1 || o.AccountSwitches != 1 || o.Model != "test", "outcome=%+v", o)
	b, err := store.Get(context.Background(), recorder.Header().Get(TraceIDHeader))
	testutil.Falsef(t, err != nil || b == nil, "diagnostics=%v err=%v", b, err)
	found, summaryFound := false, false
	for _, s := range b.Sections {
		if s.Name == "6_http_summary.json" && strings.Contains(s.Payload, `"stream_failed":true`) {
			summaryFound = true
		}
		if s.Name == "5_http_response.txt" && s.Payload == recorder.Body.String() {
			found = true
		}
	}
	testutil.False(t, !found || !summaryFound, "response diagnostic missing")
}

func TestDiagnosticsPreservesSuccessfulStructuredRawResponse(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "test:")
	handler := TraceMiddleware(Diagnostics(store, func() bool { return true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\\ndata: {}\\n\\n")
	})))
	req := httptest.NewRequest("POST", "/cline/v1/messages", strings.NewReader(`{"model":"test","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	b, err := store.Get(context.Background(), recorder.Header().Get(TraceIDHeader))
	testutil.Falsef(t, err != nil || b == nil, "diagnostics=%v err=%v", b, err)
	for _, section := range b.Sections {
		if section.Name == "5_http_response.txt" {
			testutil.Equal(t, section.Payload, recorder.Body.String())
			return
		}
	}
	t.Fatal("missing successful client response")
}
