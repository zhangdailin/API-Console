package util

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPPhasesWithoutCaptureAndReusedConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer server.Close()
	client := server.Client()
	for i := 0; i < 2; i++ {
		var phases map[string]int64
		var samples int
		ctx := WithHTTPPhaseObserver(context.Background(), func(v map[string]int64) { phases = v; samples++ })
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		req = TraceHTTPPhases(req)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		FinishHTTPPhases(req)
		FinishHTTPPhases(req)
		if samples != 1 {
			t.Fatalf("header snapshot counted %d times", samples)
		}
		if _, ok := phases["response_headers_ms"]; !ok {
			t.Fatal("missing header measurement")
		}
		if _, ok := phases["tls_ms"]; ok {
			t.Fatal("fabricated TLS stage")
		}
		if i == 1 && phases["reused_connections"] != 1 {
			t.Fatal("connection reuse lost")
		}
	}
}
