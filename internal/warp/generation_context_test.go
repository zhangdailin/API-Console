package warp

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"orchids-api/internal/debug"
	"orchids-api/internal/upstream"
)

func TestWarpConversationContextDiagnosticsPreservePresenceAndBilling(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta *warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata
		want string
	}{
		{"absent", nil, "null"},
		{"empty", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{}.Build(), "{}"},
		{"explicit false and zero", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{Summarized: proto.Bool(false), ContextWindowUsage: proto.Float32(0), TotalInputTokens: proto.Uint32(0)}.Build(), `{"summarized":false,"context_window_usage":0,"total_input_tokens":0}`},
		{"summarized", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{Summarized: proto.Bool(true), ContextWindowUsage: proto.Float32(0.75), TotalInputTokens: proto.Uint32(1000000), CreditsSpent: proto.Float32(999)}.Build(), `{"summarized":true,"context_window_usage":0.75,"total_input_tokens":1000000}`},
		{"overfull", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{ContextWindowUsage: proto.Float32(1.25)}.Build(), `{"context_window_usage":1.25}`},
		{"nan", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{Summarized: proto.Bool(true), ContextWindowUsage: proto.Float32(float32(math.NaN()))}.Build(), `{"summarized":true}`},
		{"infinity", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{ContextWindowUsage: proto.Float32(float32(math.Inf(1)))}.Build(), `{}`},
		{"negative", warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{ContextWindowUsage: proto.Float32(-0.1)}.Build(), `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finish := warpapi.ResponseEvent_StreamFinished_builder{
				Done:                      warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build(),
				ConversationUsageMetadata: tc.meta,
				RequestCharges: warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
					"private-category": warpapi.ChargedUsage_builder{DirectApiInferenceUsage: map[string]*warpapi.InferenceUsage{
						"private-model": warpapi.InferenceUsage_builder{TokenCount: warpapi.TokenCount_builder{Input: proto.Uint64(2), Output: proto.Uint64(3), InputCacheRead: proto.Uint64(0), InputCacheWrite: proto.Uint64(0)}.Build()}.Build(),
					}}.Build(),
				}}.Build(),
			}.Build()
			stream := testSSE(t, testMessages(testOutput("m", "complete answer"))) + testSSE(t, warpapi.ResponseEvent_builder{Finished: finish}.Build())
			client := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
			client.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})}
			ctx, capture := debug.WithCapture(t.Context(), "synthetic-context")
			var emitted []upstream.SSEMessage
			if err := client.SendRequestWithPayload(ctx, testRequest(), func(message upstream.SSEMessage) { emitted = append(emitted, message) }, nil); err != nil {
				t.Fatal(err)
			}
			diagnostic := capturedGenerationDiagnostic(t, capture)
			raw, err := json.Marshal(diagnostic.ConversationContext)
			if err != nil || string(raw) != tc.want || diagnostic.FinishReason != "done" {
				t.Fatalf("context presence lost: %s, want %s, error %v", raw, tc.want, err)
			}
			if diagnostic.Usage["inputTokens"] != float64(2) || diagnostic.Usage["outputTokens"] != float64(3) {
				t.Fatal("conversation context changed request usage")
			}
			public, _ := json.Marshal(emitted)
			if strings.Contains(string(public), "summarized") || strings.Contains(string(public), "1000000") || strings.Contains(string(public), "999") {
				t.Fatal("private conversation diagnostics leaked into public events or billing")
			}
		})
	}
}

func TestWarpTerminalLimitRetainsContextWithoutSuccessfulOutput(t *testing.T) {
	finish := warpapi.ResponseEvent_StreamFinished_builder{
		MaxTokenLimit:             warpapi.ResponseEvent_StreamFinished_ReachedMaxTokenLimit_builder{}.Build(),
		ConversationUsageMetadata: warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata_builder{Summarized: proto.Bool(true)}.Build(),
	}.Build()
	stream := testSSE(t, testMessages(testOutput("m", "private partial"))) + testSSE(t, warpapi.ResponseEvent_builder{Finished: finish}.Build())
	client := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
	client.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
	})}
	ctx, capture := debug.WithCapture(t.Context(), "synthetic-limit-context")
	emitted := 0
	err := client.SendRequestWithPayload(ctx, testRequest(), func(upstream.SSEMessage) { emitted++ }, nil)
	diagnostic := capturedGenerationDiagnostic(t, capture)
	if err == nil || emitted != 0 || diagnostic.Stage != "finish" || diagnostic.FinishReason != "max_token_limit" || diagnostic.ConversationContext == nil || diagnostic.ConversationContext.Summarized == nil || !*diagnostic.ConversationContext.Summarized {
		t.Fatalf("terminal limit lost diagnostics or emitted a truncated answer: %v, %+v", err, diagnostic)
	}
}
