package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type auditCancellationUpstream struct {
	wasCanceled bool
	continued   bool
}

func (c *auditCancellationUpstream) SendRequestWithPayload(ctx context.Context, _ upstream.UpstreamRequest, on func(upstream.SSEMessage), _ *debug.Logger) error {
	on(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "first"}})
	select {
	case <-ctx.Done():
		c.wasCanceled = true
		return ctx.Err()
	case <-time.After(50 * time.Millisecond):
		c.continued = true
		return nil
	}
}
func TestUpstreamReliabilityDownstreamWriteFailureCancelsUpstream(t *testing.T) {
	c := &auditCancellationUpstream{}
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = c
	w := &failingResponseWriter{err: errors.New("downstream write timeout")}
	r := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", strings.NewReader("{\"model\":\"model-a\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}"))
	h.HandleMessages(w, r)
	if !c.wasCanceled {
		t.Errorf("upstream ctx was not canceled after failed downstream write; continued=%v", c.continued)
	}
}

func TestUpstreamReliabilityHandlerQoderRetryBudget(t *testing.T) {
	old := qoder.TransientBackoff
	qoder.TransientBackoff = func(int) time.Duration { return 0 }
	defer func() { qoder.TransientBackoff = old }()
	var n atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, "{\"message\":\"provider_error\"}")
	}))
	defer server.Close()
	cfg := &config.Config{AdminPass: "test-only-password", RequestTimeout: 10, MaxRetries: 3, RetryDelay: 1, QoderInferenceURL: server.URL, QoderOpenAPIBaseURL: server.URL, QoderOAuthBaseURL: server.URL}
	acc := &store.Account{ID: 1, AccountType: "qoder", QoderAccessToken: "access", QoderRefreshToken: "refresh", QoderExpiresAt: time.Now().Add(48 * time.Hour), QoderUserID: "uid", QoderMachineID: "11111111-2222-4333-8444-555555555555", QoderModelIDs: []string{"Qwen3.7-Max"}}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = qoder.NewFromAccount(acc, cfg)
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", strings.NewReader("{\"model\":\"Qwen3.7-Max\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":false}"))
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, req)
	t.Logf("one HTTP ingress request; upstream attempts=%d response status=%d", n.Load(), rec.Code)
	if n.Load() > 4 {
		t.Errorf("one ingress request exceeded configured 4-attempt budget: got %d", n.Load())
	}
}
