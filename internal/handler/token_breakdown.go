package handler

import (
	"strings"

	"github.com/goccy/go-json"
	"orchids-api/internal/prompt"

	"orchids-api/internal/tiktoken"
)

type inputTokenBreakdown struct {
	BasePromptTokens    int
	SystemContextTokens int
	HistoryTokens       int
	ToolsTokens         int
	Total               int
}

func estimateInputTokenBreakdown(promptText string, tools []interface{}) inputTokenBreakdown {
	var bd inputTokenBreakdown
	promptTokens := tiktoken.EstimateTextTokens(promptText)
	sysText := extractTaggedContent(promptText, "sys")
	if sysText == "" {
		sysText = extractTaggedContent(promptText, "system_context")
	}
	sysTokens := tiktoken.EstimateTextTokens(sysText)
	if sysTokens > promptTokens {
		sysTokens = promptTokens
	}

	bd.SystemContextTokens = sysTokens
	bd.BasePromptTokens = promptTokens - sysTokens

	bd.ToolsTokens = estimateToolsTokens(tools)

	bd.Total = bd.BasePromptTokens + bd.SystemContextTokens + bd.HistoryTokens + bd.ToolsTokens
	return bd
}

// estimateRequestTokenBreakdown covers the complete conversational input rather
// than only the latest user text. Images use a coarse fixed budget: base64 bytes
// are not language tokens and must not inflate the estimate by megabytes.
func estimateRequestTokenBreakdown(req ClaudeRequest) inputTokenBreakdown {
	var bd inputTokenBreakdown
	for _, item := range req.System {
		bd.SystemContextTokens += tiktoken.EstimateTextTokens(item.Text)
	}
	for i, msg := range req.Messages {
		tokens := estimateMessageContentTokens(msg.Content) + tiktoken.EstimateTextTokens(msg.ReasoningContent) + 4
		if msg.Role == "system" || msg.Role == "developer" {
			bd.SystemContextTokens += tokens
		} else if i == len(req.Messages)-1 {
			bd.BasePromptTokens += tokens
		} else {
			bd.HistoryTokens += tokens
		}
	}
	bd.ToolsTokens = estimateToolsTokens(req.Tools)
	bd.Total = bd.BasePromptTokens + bd.SystemContextTokens + bd.HistoryTokens + bd.ToolsTokens
	return bd
}

func estimateMessageContentTokens(content prompt.MessageContent) int {
	if content.IsString() {
		return tiktoken.EstimateTextTokens(content.GetText())
	}
	total := 0
	for _, block := range content.GetBlocks() {
		switch block.Type {
		case "image", "image_url":
			total += 1024
		case "text":
			total += tiktoken.EstimateTextTokens(block.Text)
		case "thinking":
			total += tiktoken.EstimateTextTokens(block.Thinking)
		default:
			// Preserve structured tool inputs/results and unfamiliar block metadata in
			// the estimate instead of silently dropping whole sections of the request.
			raw, err := json.Marshal(block)
			if err == nil {
				total += tiktoken.EstimateTextTokens(string(raw))
			}
		}
	}
	return total
}

func extractTaggedContent(text string, tag string) string {
	if text == "" || tag == "" {
		return ""
	}
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"

	start := strings.Index(text, startTag)
	if start == -1 {
		return ""
	}
	start += len(startTag)
	end := strings.Index(text[start:], endTag)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}
