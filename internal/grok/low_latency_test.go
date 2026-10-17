package grok

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"testing"
	"time"
)

func TestLowLatencyBypassesQualityHold(t *testing.T) {
	effort := "high"
	req := &chatwire.Request{ReasoningEffort: &effort}
	h := NewHandler(&config.Config{}, nil)
	if !h.shouldHoldQualityTurn(req, ProviderBuild) {
		t.Fatal("default quality protection lost")
	}
	h.SetConfig(&config.Config{GrokLowLatency: true})
	if h.shouldHoldQualityTurn(req, ProviderBuild) {
		t.Fatal("low latency request still buffered")
	}
}

func TestCLIStreamingNegotiatesUnbufferedSSE(t *testing.T) {
	seen := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client, account := headerTimeoutTestClient(t, server, time.Second)
	resp, err := client.doResponsesOnceAt(context.Background(), account, "/responses", map[string]any{"model": "grok-test", "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	headers := <-seen
	if headers.Get("Accept") != "text/event-stream" || headers.Get("Accept-Encoding") != "identity" {
		t.Fatalf("unexpected headers: accept=%q encoding=%q", headers.Get("Accept"), headers.Get("Accept-Encoding"))
	}
	if headers.Get("X-XAI-Token-Auth") == "" || headers.Get("Authorization") == "" {
		t.Fatal("OAuth identity lost")
	}
}
