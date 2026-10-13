package qoder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"strings"
	"testing"
)

func TestQoderDiagnosticAttemptsCaptureSSEAndModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(envelope(`{"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":"stop"}]}`) + "event:finish\ndata:{\"firstTokenDuration\":12,\"totalDuration\":20,\"serverDuration\":2}\n\n"))
	}))
	defer srv.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	ctx, capture := debug.WithCapture(context.Background(), "latency-test")
	_, err := client.attemptChat(ctx, chatURL(srv.URL), EncodeBody([]byte(`{"messages":[],"tools":[],"session_id":"private-session","parameters":{"context_length":200000}}`)), modelEntry{Key: "qfmodel", Source: "system"}, "request", RuntimeFields{Key: "secret-key", EncryptUserInfo: "secret-info"}, client.currentCredentials(), func(upstream.SSEMessage) {})
	testutil.NoError(t, err)
	found := false
	for _, section := range capture.Bundle().Sections {
		testutil.MustNotContainAny(t, section.Payload, "secret-key", "secret-info")
		if !strings.HasSuffix(section.Name, "latency.json") {
			continue
		}
		var v map[string]interface{}
		e := json.Unmarshal([]byte(section.Payload), &v)
		testutil.NoError(t, e)
		for _, key := range []string{"first_sse_ms", "first_text_ms", "total_ms", "response_headers_ms", "remote_address", "conversation_fingerprint"} {
			_, ok := v[key]
			testutil.Falsef(t, !ok, "missing %s: %v", key, v)
		}
		testutil.Equal(t, v["model_key"], "qfmodel")
		testutil.EqualAny(t, v["upstream_firstTokenDuration"], float64(12))
		found = true
	}
	testutil.True(t, found, "missing latency artifact")
}
