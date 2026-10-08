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
	"orchids-api/internal/httpclient"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
	"orchids-api/internal/workbuddy"
)

type generationClient interface {
	SendRequestWithPayload(context.Context, upstream.UpstreamRequest, func(upstream.SSEMessage), *debug.Logger) error
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
			cfg := &config.Config{WorkBuddyBaseURL: server.URL, ClineAPIBaseURL: server.URL}
			var client generationClient
			if channel == "workbuddy" {
				client = workbuddy.NewFromAccount(&store.Account{WorkBuddyAccessToken: "test-access", WorkBuddyExpiresAt: time.Now().Add(time.Hour)}, cfg)
			} else {
				client = cline.NewFromAccount(&store.Account{ClineAccessToken: "test-access", ClineExpiresAt: time.Now().Add(time.Hour), ClineModelIDs: []string{"model-a"}}, cfg)
			}
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

type countingTail struct{ bytes, remaining int }

func (b *countingTail) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), b.remaining)
	b.bytes += n
	b.remaining -= n
	return n, nil
}
func (*countingTail) Close() error { return nil }

func TestTerminalDrainReadsFullTailAndSkipsHTTP2OrCancelled(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		major     int
		cancelled bool
		close     bool
		want      int
	}{{"full HTTP1 tail", 1, false, false, 192 << 10}, {"HTTP2", 2, false, false, 0}, {"cancelled", 1, true, false, 0}, {"connection close", 1, false, true, 0}} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario.cancelled {
				cancel()
			}
			body := &countingTail{remaining: 192 << 10}
			httpclient.DrainTerminalResponse(ctx, &http.Response{ProtoMajor: scenario.major, Close: scenario.close, Body: body})
			if body.bytes != scenario.want {
				t.Fatalf("read %d tail bytes; want %d", body.bytes, scenario.want)
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
			cfg := &config.Config{WorkBuddyBaseURL: server.URL, ClineAPIBaseURL: server.URL}
			var client generationClient
			if channel == "workbuddy" {
				client = workbuddy.NewFromAccount(&store.Account{WorkBuddyAccessToken: "test-access", WorkBuddyExpiresAt: time.Now().Add(time.Hour)}, cfg)
			} else {
				client = cline.NewFromAccount(&store.Account{ClineAccessToken: "test-access", ClineExpiresAt: time.Now().Add(time.Hour), ClineModelIDs: []string{"model-a"}}, cfg)
			}
			for i := 0; i < 2; i++ {
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
