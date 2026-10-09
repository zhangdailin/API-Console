package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestResponsesBridgeStreamCompletesAndCloses(t *testing.T) {
	bridge := responses.ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-test\",\"model\":\"qwen3.8-flash\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}, responses.BridgeOptions{})
	server := httptest.NewServer(bridge)
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Post(server.URL+"/qoder/v1/responses", "application/json", strings.NewReader(`{"model":"qwen3.8-flash","input":"hi","stream":true}`))
	testutil.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	testutil.NoError(t, err, "stream did not terminate cleanly: %v")
	testutil.Falsef(t, resp.StatusCode != 200 || strings.Count(string(body), "event: response.completed") != 1 || !strings.Contains(string(body), "response.output_text.delta"), "incomplete stream: %s", body)
}

// Queue refusal must remain a retryable HTTP response, not become a malformed
// successful Responses object or a stream that never closes.
func TestResponsesBridgePreservesQueueRefusal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			bridge := responses.ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"upstream queue busy"}}`))
			}, responses.BridgeOptions{})
			req := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"qwen3.8-flash","input":"hi","stream":%v}`, stream)))
			rec := httptest.NewRecorder()
			bridge(rec, req)
			testutil.Falsef(t, rec.Code != 429 || rec.Header().Get("Retry-After") != "30" || !strings.Contains(rec.Body.String(), "rate_limit_error"), "queue response lost: status=%d header=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
		})
	}
}

func TestResponsesBridgePropagatesQueueCancellation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			finished := make(chan struct{})
			bridge := responses.ResponsesBridgeHandler(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"request canceled"}}`))
			}, responses.BridgeOptions{})
			req := httptest.NewRequest(http.MethodPost, "/qoder/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"qwen3.8-flash","input":"hi","stream":%v}`, stream))).WithContext(ctx)
			go func() { defer close(finished); bridge(httptest.NewRecorder(), req) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("inner handler never started")
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("bridge did not return after cancellation")
			}
		})
	}
}
