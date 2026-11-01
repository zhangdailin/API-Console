package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
	"strings"
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
