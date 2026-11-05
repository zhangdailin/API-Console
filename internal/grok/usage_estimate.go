package grok

import (
	"orchids-api/internal/chatwire"
	"orchids-api/internal/grok/grokbase"
)

// buildChatUsagePayload estimates usage for a Build chat turn the upstream did
// not report usage for.
func buildChatUsagePayload(req *chatwire.Request, finalContent string, toolCalls []map[string]any) map[string]any {
	var prompt grokbase.UsageEstimate
	if req != nil {
		prompt = grokbase.EstimatePrompt(req.Messages, req.Tools, req.ToolChoice)
	}
	return grokbase.ChatUsagePayload(prompt, finalContent, toolCalls)
}
