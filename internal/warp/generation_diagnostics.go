package warp

import (
	"context"
	"errors"
	"math"
	"net"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"orchids-api/internal/httpclient"
)

var errGenerationTimeout = errors.New("warp generation transport timed out")

func isGenerationTimeout(err error) bool {
	if errors.Is(err, errGenerationTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, httpclient.ErrStreamIdle("warp")) {
		return true
	}
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

func generationTimeoutReason(err error) string {
	switch {
	case errors.Is(err, httpclient.ErrStreamIdle("warp")):
		return "stream_idle"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_deadline"
	case isGenerationTimeout(err):
		return "transport_timeout"
	}
	return ""
}

// Conversation-scoped observations are diagnostic only, never request billing.
// Pointers preserve unknown separately from explicit false and zero.
type conversationContextDiagnostic struct {
	Summarized         *bool    `json:"summarized,omitempty"`
	ContextWindowUsage *float32 `json:"context_window_usage,omitempty"`
	TotalInputTokens   *uint32  `json:"total_input_tokens,omitempty"`
}

func observeConversationContext(meta *warpapi.ResponseEvent_StreamFinished_ConversationUsageMetadata) *conversationContextDiagnostic {
	if meta == nil {
		return nil
	}
	observed := &conversationContextDiagnostic{}
	if meta.HasSummarized() {
		value := meta.GetSummarized()
		observed.Summarized = &value
	}
	if value := meta.GetContextWindowUsage(); meta.HasContextWindowUsage() && value >= 0 && !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0) {
		observed.ContextWindowUsage = &value
	}
	if meta.HasTotalInputTokens() {
		value := meta.GetTotalInputTokens()
		observed.TotalInputTokens = &value
	}
	return observed
}

func observedFinishReason(finish *warpapi.ResponseEvent_StreamFinished) string {
	switch finish.WhichReason() {
	case warpapi.ResponseEvent_StreamFinished_Done_case:
		return "done"
	case warpapi.ResponseEvent_StreamFinished_MaxTokenLimit_case:
		return "max_token_limit"
	case warpapi.ResponseEvent_StreamFinished_ContextWindowExceeded_case:
		return "context_window_exceeded"
	case warpapi.ResponseEvent_StreamFinished_QuotaLimit_case:
		return "quota_limit"
	case warpapi.ResponseEvent_StreamFinished_LlmUnavailable_case:
		return "llm_unavailable"
	case warpapi.ResponseEvent_StreamFinished_InternalError_case:
		return "internal_error"
	case warpapi.ResponseEvent_StreamFinished_InvalidApiKey_case:
		return "invalid_api_key"
	case warpapi.ResponseEvent_StreamFinished_ChatgptSubscriptionError_case:
		return "chatgpt_subscription_error"
	case warpapi.ResponseEvent_StreamFinished_Other_case:
		return "other"
	default:
		return "unspecified"
	}
}
