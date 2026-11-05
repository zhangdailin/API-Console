package web

import "orchids-api/internal/grok/grokbase"

// buildChatUsagePayload estimates usage for a Web chat turn; grok.com reports
// none.
func buildChatUsagePayload(req *ChatCompletionsRequest, finalContent string, toolCalls []map[string]interface{}) map[string]interface{} {
	var prompt grokbase.UsageEstimate
	if req != nil {
		prompt = grokbase.EstimatePrompt(req.Messages, req.Tools, req.ToolChoice)
	}
	return grokbase.ChatUsagePayload(prompt, finalContent, toolCalls)
}

// buildImageUsagePayload reports the prompt tokens the gateway can actually
// account for. Image generation is not a token-metered operation upstream, so
// the completion side is reported as zero rather than a fabricated per-image
// constant (grok2api reports 0 / omits usage entirely).
func buildImageUsagePayload(prompt string, imageCount int) map[string]interface{} {
	promptTokens := grokbase.ApproxTokenCount(prompt)
	completionTokens := 0
	return map[string]interface{}{
		"total_tokens":  promptTokens + completionTokens,
		"input_tokens":  promptTokens,
		"output_tokens": completionTokens,
		"input_tokens_details": map[string]interface{}{
			"text_tokens":  promptTokens,
			"image_tokens": 0,
		},
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": 0,
			"text_tokens":   promptTokens,
			"audio_tokens":  0,
			"image_tokens":  0,
		},
		"completion_tokens_details": map[string]interface{}{
			"text_tokens":      completionTokens,
			"audio_tokens":     0,
			"reasoning_tokens": 0,
		},
	}
}
