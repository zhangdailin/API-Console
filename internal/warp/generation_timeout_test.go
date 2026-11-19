package warp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/upstream"
)

func capturedGenerationDiagnostic(t *testing.T, capture *debug.Capture) generationDiagnostic {
	t.Helper()
	for _, section := range capture.Bundle().Sections {
		if section.Name == "3_warp_diagnostic.json" {
			var diagnostic generationDiagnostic
			if err := json.Unmarshal([]byte(section.Payload), &diagnostic); err != nil {
				t.Fatal(err)
			}
			return diagnostic
		}
	}
	t.Fatal("generation diagnostic missing")
	return generationDiagnostic{}
}

func TestWarpGenerationTimeoutPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		want       time.Duration
	}{
		{"default", 0, config.InferenceRequestTimeout},
		{"shorter operation", 30, 30 * time.Second},
		{"bounded", 999999, config.InferenceRequestTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("https://private.invalid")
			cfg.RequestTimeout = tc.configured
			client := NewFromAccount(testAccount(), cfg)
			if client.generationTimeout != tc.want || client.http.Timeout != tc.want {
				t.Fatal("generation deadline differs from the shared inference policy")
			}
			transport := client.http.Transport.(*http.Transport)
			if transport.ResponseHeaderTimeout != min(tc.want, config.SharedStreamIdleTimeout) {
				t.Fatal("unexpected shorter response-header deadline")
			}
		})
	}
}

func TestWarpGenerationProgressSurvivesIdleBudget(t *testing.T) {
	frames := make([]string, 8)
	for i := range frames {
		frames[i] = testSSE(t, testMessages(testOutput("m", strings.Repeat("x", i+1))))
	}
	done := testSSE(t, testDone())
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			if _, err := io.WriteString(w, frame); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(40 * time.Millisecond):
			}
		}
		_, _ = io.WriteString(w, done)
	}))
	defer server.Close()
	client := NewFromAccount(testAccount(), testConfig(server.URL))
	client.streamIdleTimeout = 200 * time.Millisecond
	client.generationTimeout = 3 * time.Second
	var text string
	if err := client.SendRequestWithPayload(t.Context(), testRequest(), func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			text += message.Event["delta"].(string)
		}
	}, nil); err != nil || text != strings.Repeat("x", 8) || calls.Load() != 1 {
		t.Fatalf("progressing response was cut off or replayed: %v, text=%q, calls=%d", err, text, calls.Load())
	}
}

func TestWarpGenerationTimeoutsDoNotEmitPartialOrRetry(t *testing.T) {
	prefix := testSSE(t, testMessages(testOutput("m", "private partial answer")))
	for _, tc := range []struct {
		name           string
		idle, deadline time.Duration
		reason         string
	}{
		{"idle with heartbeats", 60 * time.Millisecond, 3 * time.Second, "stream_idle"},
		{"request deadline", time.Second, 60 * time.Millisecond, "request_deadline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, prefix)
				w.(http.Flusher).Flush()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-r.Context().Done():
						return
					case <-ticker.C:
						if _, err := io.WriteString(w, ": heartbeat\n\n"); err != nil {
							return
						}
						w.(http.Flusher).Flush()
					}
				}
			}))
			defer server.Close()
			client := NewFromAccount(testAccount(), testConfig(server.URL))
			client.streamIdleTimeout, client.generationTimeout = tc.idle, tc.deadline
			ctx, capture := debug.WithCapture(t.Context(), "synthetic-timeout")
			emitted := 0
			err := client.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted++ }, nil)
			diagnostic := capturedGenerationDiagnostic(t, capture)
			if err == nil || emitted != 0 || calls.Load() != 1 || diagnostic.Stage != "timeout" || diagnostic.ParserCategory != 8 || diagnostic.TimeoutReason != tc.reason {
				t.Fatalf("timeout lost its cause, emitted partial output or retried: %v, %+v, emitted=%d, calls=%d", err, diagnostic, emitted, calls.Load())
			}
			if tc.reason == "request_deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request deadline was not preserved: %v", err)
			}
		})
	}
}

func TestWarpGenerationTransportTimeoutIsSanitized(t *testing.T) {
	client := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
	calls := 0
	client.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, &url.Error{Op: "Post", URL: "https://private.invalid/secret", Err: &net.DNSError{Err: "private-token", IsTimeout: true}}
	})}
	ctx, capture := debug.WithCapture(t.Context(), "synthetic-transport-timeout")
	err := client.SendRequestWithPayload(ctx, testRequest(), nil, nil)
	diagnostic := capturedGenerationDiagnostic(t, capture)
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != 502 || diagnostic.Stage != "timeout" || diagnostic.TimeoutReason != "transport_timeout" || calls != 1 {
		t.Fatalf("wrong transport timeout result: %v, %+v", err, diagnostic)
	}
	raw, _ := json.Marshal(capture.Bundle())
	if strings.Contains(string(raw)+fmt.Sprint(err), "private") {
		t.Fatal("raw transport error escaped sanitization")
	}
}

func TestWarpGenerationCallerCancellationIsPreserved(t *testing.T) {
	prefix := testSSE(t, testMessages(testOutput("m", "private partial")))
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, prefix)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	client := NewFromAccount(testAccount(), testConfig(server.URL))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		select {
		case <-started:
			cancel()
		case <-ctx.Done():
		}
	}()
	emitted := 0
	err := client.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted++ }, nil)
	if !errors.Is(err, context.Canceled) || emitted != 0 {
		t.Fatalf("caller cancellation was lost or partial output emitted: %v", err)
	}
}
