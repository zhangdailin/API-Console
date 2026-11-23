package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/middleware"
)

func TestGrokWebIncompleteStreamDoesNotClaimSuccess(t *testing.T) {
	for _, c := range []struct {
		name   string
		frames []string
	}{
		{name: "empty"},
		{name: "partial", frames: []string{line(map[string]any{"token": "partial"})}},
		{name: "interrupted_json", frames: []string{`{"result":{"response":`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			stream := runStream(t, nil, upstreamLines(c.frames...))
			if stream.finish != "" || stream.done || len(stream.errors) != 1 {
				t.Fatalf("stream did not emit exactly one failure without a success terminal, stop=%q done=%v errors=%d", stream.finish, stream.done, len(stream.errors))
			}
			h := &Handler{}
			req := &ChatCompletionsRequest{Request: chatwire.Request{Model: "grok-4"}}
			w := httptest.NewRecorder()
			h.collectChat(t.Context(), w, req, "", "", false, upstreamLines(c.frames...), nil)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("non-streaming incomplete response must be a bad gateway, status=%d", w.Code)
			}
		})
	}
}

type chatJournalSink []audit.Event

func (s *chatJournalSink) Log(_ context.Context, e audit.Event) { *s = append(*s, e) }

func TestGrokWebJournalKeepsRequestContext(t *testing.T) {
	for _, stream := range []bool{false, true} {
		var sink chatJournalSink
		middleware.SetRequestAuditLogger(&sink)
		t.Cleanup(func() { middleware.SetRequestAuditLogger(nil) })
		h := &Handler{}
		h.SetAuditLogger(middleware.ObserveAuditLogger(&sink))
		route := middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{ID: 7}, nil
		}, func(w http.ResponseWriter, r *http.Request) {
			req := &ChatCompletionsRequest{Request: chatwire.Request{Model: "grok-4", Stream: stream}}
			body := upstreamLines(line(map[string]any{"modelResponse": map[string]any{"message": "fixture response"}}))
			if stream {
				h.streamChat(r.Context(), w, req, "", "", false, body, nil)
			} else {
				h.collectChat(r.Context(), w, req, "", "", false, body, nil)
			}
		})
		proxy := middleware.TraceMiddleware(middleware.LoggingMiddleware(route))
		r := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer sk-fixture-journal-key")
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		if len(sink) != 1 {
			t.Fatalf("expected one journal row, stream=%v count=%d", stream, len(sink))
		}
		event := sink[0]
		if event.Action != "grok_request" || event.RequestID == "" || event.APIKeyID != 7 || event.TotalTokens == 0 {
			t.Fatalf("journal lost request identity, key attribution or usage, stream=%v", stream)
		}
	}
}
