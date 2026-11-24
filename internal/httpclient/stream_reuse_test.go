package httpclient_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/cline"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/workbuddy"
)

type generationClient interface {
	SendRequestWithPayload(context.Context, upstream.UpstreamRequest, func(upstream.SSEMessage), *debug.Logger) error
}

func newStreamFixtureClient(channel, url string) generationClient {
	cfg := &config.Config{WorkBuddyBaseURL: url, ClineAPIBaseURL: url}
	expiry := time.Now().Add(time.Hour)
	if channel == "workbuddy" {
		return workbuddy.NewFromAccount(&store.Account{WorkBuddyAccessToken: "test-access", WorkBuddyExpiresAt: expiry}, cfg)
	}
	return cline.NewFromAccount(&store.Account{ClineAccessToken: "test-access", ClineExpiresAt: expiry, ClineModelIDs: []string{`{"id":"model-a"}`}}, cfg)
}

func TestTerminalTailHonorsOriginalRequestCancellation(t *testing.T) {
	for _, channel := range []string{"workbuddy", "cline"} {
		t.Run(channel, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			client := newStreamFixtureClient(channel, server.URL)
			parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			err := client.SendRequestWithPayload(parent, upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
			if err != nil || time.Since(started) > time.Second || parent.Err() != context.DeadlineExceeded {
				t.Fatalf("terminal tail must stop with original request deadline while preserving completion: error=%v elapsed=%v parent=%v", err, time.Since(started), parent.Err())
			}
		})
	}
}

func TestTerminalTailHasIndependentDeadline(t *testing.T) {
	for _, channel := range []string{"workbuddy", "cline"} {
		t.Run(channel, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			client := newStreamFixtureClient(channel, server.URL)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := time.Now()
			err := client.SendRequestWithPayload(parent, upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
			if err != nil || time.Since(started) > 2*time.Second || parent.Err() != nil {
				t.Fatalf("terminal tail must stop within cleanup budget while preserving completion: error=%v elapsed=%v parent=%v", err, time.Since(started), parent.Err())
			}
		})
	}
}

func TestSharedStreamsReuseHTTP1AfterDone(t *testing.T) {
	for _, channel := range []string{"workbuddy", "cline"} {
		t.Run(channel, func(t *testing.T) {
			var mu sync.Mutex
			connections := make(map[string]bool)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				connections[r.RemoteAddr] = true
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				w.(http.Flusher).Flush()
				// DONE and the HTTP chunk terminator arrive separately in a real stream.
				select {
				case <-time.After(350 * time.Millisecond):
					_, _ = io.WriteString(w, strings.Repeat("\n", 128<<10))
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			client := newStreamFixtureClient(channel, server.URL)
			for i := range 2 {
				text := ""
				ctx, cancel := context.WithCancel(context.Background())
				err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, func(event upstream.SSEMessage) {
					if event.Type == "model.text-delta" {
						text += event.Event["delta"].(string)
					}
					// Streaming clients may close the request as soon as they see DONE.
					if event.Type == "model.finish" {
						cancel()
					}
				}, nil)
				cancel()
				if err != nil || text != "OK" {
					t.Fatalf("request %d: text=%q error=%v", i, text, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(connections) != 1 {
				t.Fatalf("two completed streams used %d TCP connections; want one reused connection", len(connections))
			}
		})
	}
}
