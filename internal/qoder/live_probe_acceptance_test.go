//go:build live

package qoder

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestProbeGenerationRequiresOutputAndCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"text and finish", envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`) + "event:finish\n\n", 200, false},
		{"finish alone", "event:finish\n\n", 200, true},
		{"usage alone", envelope(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`) + "event:finish\n\n", 200, true},
		{"reasoning alone", envelope(`{"choices":[{"delta":{"reasoning_content":"thinking"},"finish_reason":"stop"}]}`) + "event:finish\n\n", 200, true},
		{"truncated text", envelope(`{"choices":[{"delta":{"content":"OK"}}]}`), 200, true},
		{"HTTP rejection", `{"code":"10605","message":"queue"}`, 401, true},
		{"SSE rejection", "data: {\"success\":false,\"code\":\"10605\",\"message\":\"queue\"}\n\nevent:finish\n\n", 200, true},
		{"auth rejection", `{"message":"login expired"}`, 401, true},
		{"provider error", `{"message":"provider error"}`, 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client := NewFromAccount(signedTestAccount(), &config.Config{QoderInferenceURL: server.URL, QoderOpenAPIBaseURL: server.URL})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := probeGeneration(ctx, client, upstream.UpstreamRequest{Model: "qwen3.8-max", Prompt: "Reply OK."})
			testutil.Equal(t, err != nil, tc.wantError)
			testutil.Equal(t, calls, 1)
			testutil.Equal(t, result.protocol, "HTTP/1.1")
		})
	}
}
