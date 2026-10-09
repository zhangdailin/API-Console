package cline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestStreamStallHonorsIdleBudgetUnderLongParentDeadline(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer s.Close()
	c := NewFromAccount(&store.Account{ClineAccessToken: "access", ClineModelIDs: []string{`{"id":"model-a"}`}}, &config.Config{ClineAPIBaseURL: s.URL})
	c.streamIdle = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := c.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	if !errors.Is(err, httpclient.ErrStreamIdle("cline")) {
		t.Fatalf("idle stream error=%v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("local stream timeout canceled the parent")
	}
}
