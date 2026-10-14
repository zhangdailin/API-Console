package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestCLIDiagnosticsCaptureRequestResponseAndRetry(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			io.WriteString(w, `{"error":"limited"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	}))
	defer server.Close()
	c := NewCLIClient(&config.Config{})
	c.httpClient = server.Client()
	ctx, capture := debug.WithCapture(context.Background(), "cli")
	for range 2 {
		resp, err := c.request(ctx, &store.Account{OAuthAccessToken: "fake-private-token", OAuthExpiresAt: time.Now().Add(time.Hour)}, "POST", server.URL, []byte(`{"input":"hello"}`), nil)
		testutil.NoError(t, err)
		io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	sections := map[string]string{}
	for _, s := range capture.Bundle().Sections {
		sections[s.Name] = s.Payload
		testutil.MustNotContain(t, s.Payload, "fake-private-token")
	}
	for _, tc := range []struct{ name, want string }{
		{"upstream_001_request.json", "hello"}, {"upstream_001_result.json", "429"}, {"upstream_001_response.txt", "limited"},
		{"upstream_002_request.json", "hello"}, {"upstream_002_result.json", "200"}, {"upstream_002_response.txt", "response.output_text.delta"},
	} {
		testutil.MustContain(t, sections[tc.name], tc.want)
	}
}
