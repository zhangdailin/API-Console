package errors

import (
	"orchids-api/internal/testutil"
	"testing"
)

func TestClassifyUpstreamError(t *testing.T) {
	tests := []struct {
		name         string
		errStr       string
		wantCategory string
		wantRetry    bool
		wantSwitch   bool
	}{
		{
			name:         "model not found is client error",
			errStr:       "workbuddy API error: message=Model not found, please try another model",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "no implementation available is client error",
			errStr:       "workbuddy API error: code=no_implementation_available, status=502, message=No implementation available for interface `workbuddy-chat-completion`.",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "insufficient funds is quota exhausted",
			errStr:       "workbuddy API error: code=insufficient_funds, status=402, message=Available funding is insufficient for this request.",
			wantCategory: "quota_exhausted",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "quota limit is quota exhausted",
			errStr:       "upstream stream finished with quota_limit: no remaining quota",
			wantCategory: "quota_exhausted",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "no ai credits is quota exhausted",
			errStr:       `upstream stream request failed: HTTP 429 [OUT_OF_CREDITS]: {"error":"No AI credits remaining"}`,
			wantCategory: "quota_exhausted",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "context window is client error",
			errStr:       "upstream stream finished with context_window_exceeded: input is too long",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
		{
			name:         "invalid api key switches account",
			errStr:       "upstream stream finished with invalid_api_key: provider=openai model=gpt-test",
			wantCategory: "auth",
			wantRetry:    true,
			wantSwitch:   true,
		},
		{
			name:         "duplicate request is not retried",
			errStr:       "upstream duplicate request",
			wantCategory: "client",
		},
		{
			name:         "max token limit is client error",
			errStr:       "upstream stream finished with max_token_limit: maximum output tokens reached",
			wantCategory: "client",
			wantRetry:    false,
			wantSwitch:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyUpstreamError(tt.errStr)
			testutil.Falsef(t, got.Category != tt.wantCategory || got.Retryable != tt.wantRetry || got.SwitchAccount != tt.wantSwitch, "ClassifyUpstreamError(%q) = %#v, want category=%q retry=%v switch=%v", tt.errStr, got, tt.wantCategory, tt.wantRetry, tt.wantSwitch)
		})
	}
}
