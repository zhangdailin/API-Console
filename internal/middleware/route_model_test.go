package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/upstream"
)

func TestRequestJournalResponseModelAcrossChannelsAndProtocols(t *testing.T) {
	for _, channel := range []string{"warp", "cline", "workbuddy", "grok", "doubao"} {
		for _, protocol := range []string{"chat/completions", "messages", "responses"} {
			t.Run(channel+"/"+protocol, func(t *testing.T) {
				journal := &diagnosticJournal{}
				previous := requestJournal
				requestJournal = journal
				defer func() { requestJournal = previous }()
				h := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstream.NoteUpstreamCall(r.Context(), "https://gateway.example/response?credential=private", "sent-alias")
					upstream.NoteServedModel(r.Context(), "reported-model")
					_, _ = w.Write([]byte(`{"model":"requested-alias"}`))
				})))
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/"+channel+"/v1/"+protocol, strings.NewReader(`{"model":"requested-alias"}`)))
				if rr.Body.String() != `{"model":"requested-alias"}` || len(journal.events) != 1 {
					t.Fatal("route observation changed response or journal count")
				}
				m := journal.events[0].Metadata
				if m[upstream.RouteServedModel] != "reported-model" || m[upstream.RouteSentModel] != "sent-alias" || m[upstream.RouteModelSource] != "upstream_response" || m[upstream.RouteModelStatus] != "reported" {
					t.Fatal("response identity missing from journal")
				}
			})
		}
	}
}

func TestRequestJournalClearsPreviousAttemptResponseFacts(t *testing.T) {
	journal := &diagnosticJournal{}
	previous := requestJournal
	requestJournal = journal
	defer func() { requestJournal = previous }()
	h := TraceMiddleware(LoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.NoteUpstreamCall(r.Context(), "https://first.example/chat", "first")
		upstream.NoteServedModel(r.Context(), "failed-attempt-model")
		upstream.NoteServedProvider(r.Context(), "previous-provider", 2)
		upstream.NoteModelFallback(r.Context(), true)
		upstream.NoteUpstreamCall(r.Context(), "https://last.example/chat", "last")
		_, _ = w.Write([]byte("ok"))
	})))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/cline/v1/messages", strings.NewReader("{}")))
	if len(journal.events) != 1 {
		t.Fatal("missing final request row")
	}
	m := journal.events[0].Metadata
	for _, k := range []string{upstream.RouteServedModel, upstream.RouteProvider, upstream.RouteModelSource, upstream.RouteModelFallback, upstream.RouteProviderAttempts} {
		if _, exists := m[k]; exists {
			t.Fatalf("stale response field persisted: %s", k)
		}
	}
	if m[upstream.RouteModelStatus] != "unreported" || m[upstream.RouteSentModel] != "last" {
		t.Fatal("unreported last attempt was replaced with inferred identity")
	}
}
