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
	"orchids-api/internal/debug"
)

func TestDiagnosticsCapturesAllChannelResponses(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	store := debug.NewDiagnosticStore(client, "channels:")
	for _, provider := range []string{"workbuddy", "cline", "grok"} {
		for _, endpoint := range []string{"responses", "responses/compact", "messages", "chat/completions"} {
			for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
				path := "/" + provider + "/v1/" + endpoint
				h := TraceMiddleware(Diagnostics(store, func() bool { return true })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if debug.FromContext(r.Context()) == nil {
						t.Fatalf("capture missing: %s", path)
					}
					_, _ = io.ReadAll(r.Body)
					attempt := debug.BeginUpstream(r.Context(), "POST", "https://example.test/inference", nil, nil)
					attempt.Response(&http.Response{StatusCode: status, Header: http.Header{}}, nil)
					body := attempt.CaptureBody(io.NopCloser(strings.NewReader("upstream-result")))
					_, _ = io.ReadAll(body)
					_ = body.Close()
					w.WriteHeader(status)
					_, _ = io.WriteString(w, "client-result")
				})))
				recorder := httptest.NewRecorder()
				h.ServeHTTP(recorder, httptest.NewRequest("POST", path, strings.NewReader(`{"model":"test"}`)))
				bundle, err := store.Get(context.Background(), recorder.Header().Get(DiagnosticRequestIDHeader))
				if err != nil || bundle == nil {
					t.Fatalf("%s status %d: bundle=%v err=%v", path, status, bundle, err)
				}
				found := map[string]bool{}
				for _, section := range bundle.Sections {
					found[section.Name] = true
				}
				for _, name := range []string{"1_http_request.json", "upstream_001_request.json", "upstream_001_response.txt", "upstream_001_result.json", "5_http_response.txt", "6_http_summary.json"} {
					if !found[name] {
						t.Fatalf("%s missing %s", path, name)
					}
				}
			}
		}
		for _, endpoint := range []string{"responses/id", "responses/id/cancel", "responses/id/input_items", "models", "messages/count_tokens"} {
			path := "/" + provider + "/v1/" + endpoint
			if requestChannel(path) != HTTPChannel {
				t.Fatalf("non-generation route classified as inference: %s", path)
			}
		}
	}
}
