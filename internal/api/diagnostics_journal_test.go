package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/middleware"
	"orchids-api/internal/testutil"
)

func TestInvalidJSONDiagnosticIsReachableFromJournal(t *testing.T) {
	s, _ := newTestStore(t, "invalid-json-diagnostic:")
	d := debug.NewDiagnosticStore(s.RedisClient(), s.RedisPrefix())
	a := &API{store: s}
	a.SetDiagnosticStore(d)
	journal := audit.NewRedisLogger(s.RedisClient(), s.RedisPrefix(), 100)
	middleware.SetRequestAuditLogger(journal)
	defer middleware.SetRequestAuditLogger(nil)
	h := new(handler.Handler)
	h.SetAuditLogger(middleware.ObserveAuditLogger(journal))
	wrapped := middleware.TraceMiddleware(middleware.Diagnostics(d, func() bool { return true })(middleware.LoggingMiddleware(http.HandlerFunc(h.HandleMessages))))
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, httptest.NewRequest("POST", "/workbuddy/v1/messages", strings.NewReader("bad-json")))
	journal.Close() // Flush the asynchronous audit writer before querying the API.
	testutil.Equal(t, rec.Code, 400)
	list := journalRequest(t, a, "?kind=request&channel=workbuddy")
	rows := list["data"].([]any)
	testutil.Equal(t, len(rows), 1)
	row := rows[0].(map[string]any)
	event := row["event"].(map[string]any)
	id := rec.Header().Get(middleware.DiagnosticRequestIDHeader)
	testutil.Fail(t, event["request_id"] != id || row["diagnostics"] == nil, row)
	detail := httptest.NewRecorder()
	a.HandleJournalDiagnostics(detail, httptest.NewRequest("GET", "/api/journal/diagnostics?request_id="+id, nil))
	testutil.Fail(t, detail.Code != 200 || !strings.Contains(detail.Body.String(), "bad-json"), detail.Code, detail.Body.String())
}
