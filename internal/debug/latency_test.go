package debug

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestLatencyCapturesReusedConnectionsAndSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer server.Close()
	ctx, capture := WithCapture(context.Background(), "latency")
	client := server.Client()
	for range 2 {
		req, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
		req.Header.Set("Cosy-Key", "secret-key")
		req.Header.Set("Cosy-User", "private-user")
		a := BeginUpstream(ctx, req.Method, server.URL, req.Header, nil)
		traced, l := a.Trace(ctx, map[string]interface{}{"model_key": "qfmodel"})
		resp, err := client.Do(req.WithContext(traced))
		testutil.NoError(t, err)
		l.Response(resp)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		l.Mark("first_sse_ms")
		l.Mark("first_sse_ms")
		l.Finish(nil)
	}
	bundle := capture.Bundle()
	count := 0
	for _, s := range bundle.Sections {
		testutil.MustNotContainAny(t, s.Payload, "secret-key", "private-user")
		if !strings.HasSuffix(s.Name, "latency.json") {
			continue
		}
		count++
		var v map[string]interface{}
		testutil.NoError(t, json.Unmarshal([]byte(s.Payload), &v))
		if count == 2 {
			testutil.Equal(t, v["connection_reused"], true)
			_, ok := v["dns_ms"]
			testutil.False(t, ok, "invented DNS timing")
		}
		testutil.EqualAny(t, v["http_status"], float64(200))
		testutil.Equal(t, v["http_protocol"], "HTTP/1.1")
	}
	testutil.Equal(t, count, 2)
}
func TestLatencyDisabledAndWaitCancellation(t *testing.T) {
	ctx := context.Background()
	var a *UpstreamAttempt
	traced, l := a.Trace(ctx, nil)
	testutil.False(t, traced != ctx || l != nil, "traced disabled capture")
	l.Mark("x")
	l.Finish(nil)
	ctx, c := WithCapture(ctx, "cancelled")
	RecordWait(ctx, "upstream_queue", time.Second, 10*time.Millisecond, true)

	found := false
	for _, s := range c.Bundle().Sections {
		if strings.Contains(s.Payload, `"actual_wait_ms": 10`) && strings.Contains(s.Payload, `"cancelled": true`) {
			found = true
		}
	}
	testutil.True(t, found, "wait lost")
}
