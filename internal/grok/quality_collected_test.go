package grok

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestCollectedQualityDoesNotInventStreamTiming(t *testing.T) {
	cases := []struct {
		name, reasoning, encrypted    string
		reasoningTokens, outputTokens int
		withheld                      bool
		reason                        string
	}{
		{name: "plaintext with high reasoning ratio", reasoning: "considering the request", reasoningTokens: 90, outputTokens: 100},
		{name: "cipher-only with billed reasoning", encrypted: strings.Repeat("x", 512), reasoningTokens: 100, outputTokens: 110},
		{name: "short cipher with billed reasoning", encrypted: strings.Repeat("x", 216), reasoningTokens: 32, outputTokens: 40},
		{name: "missing reasoning", outputTokens: 10, withheld: true, reason: qualityReasonNoReasoning},
		{name: "cipher-only with zero reasoning bill", encrypted: strings.Repeat("x", 512), outputTokens: 10, withheld: true, reason: qualityReasonCipherDrool},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
				return &config.Config{GrokCLIBaseURL: base}
			}, "jwt-user-healthy")
			output := []any{}
			if tc.reasoning != "" || tc.encrypted != "" {
				output = append(output, map[string]any{
					"type": "reasoning", "encrypted_content": tc.encrypted,
					"summary": []any{map[string]any{"type": "summary_text", "text": tc.reasoning}},
				})
			}
			output = append(output, map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": "an answer containing enough text"}},
			})
			body, err := json.Marshal(map[string]any{
				"status": "completed", "output": output,
				"usage": map[string]any{
					"input_tokens": 10, "output_tokens": tc.outputTokens,
					"output_tokens_details": map[string]any{"reasoning_tokens": tc.reasoningTokens},
				},
			})
			testutil.NoError(t, err, "marshal response: %v")
			effort := "high"
			req := &chatwire.Request{Model: "grok-4.5", ReasoningEffort: &effort}
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body)))}
			rec := httptest.NewRecorder()
			parked, withheld := h.finishUpstreamChat(context.Background(), rec, req, &chatAccountSession{acc: accounts[0]}, nil, ProviderBuild, "/responses", func() http.Header { return http.Header{} }, nil, response, nil, 1)
			testutil.Equal(t, withheld, tc.withheld)
			checks := journal.byAction("grok_quality_check")
			testutil.EqualAny(t, checks[0].Metadata["first_visible_ms"], int64(-1))
			if tc.withheld {
				testutil.True(t, parked != nil, "degraded response must remain held")
				testutil.Equal(t, rec.Body.Len(), 0)
				degraded := journal.byAction("grok_quality_degraded")
				testutil.Equal(t, len(degraded), 1)
				testutil.EqualAny(t, degraded[0].Metadata["reason"], tc.reason)
				testutil.EqualAny(t, degraded[0].Metadata["first_visible_ms"], int64(-1))
			} else {
				testutil.MustContain(t, rec.Body.String(), "an answer containing enough text")
				testutil.Equal(t, len(checks), 2)
				testutil.Equal(t, checks[1].Status, "passed")
				testutil.EqualAny(t, checks[1].Metadata["first_visible_ms"], int64(-1))
				testutil.Equal(t, len(journal.byAction("grok_quality_guard")), 0)
				saved, err := h.lb.Store.GetAccount(context.Background(), accounts[0].ID)
				testutil.NoError(t, err)
				testutil.Equal(t, saved.QualityFailures, 0)
				testutil.True(t, saved.QualityCooldownUntil.IsZero(), "passed response must not park its account")
			}
		})
	}
}
