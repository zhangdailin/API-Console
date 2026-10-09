package handler

import (
	"strings"

	"encoding/json"
)

func mapStopReasonToOpenAIFinishReason(stopReason string) *string {
	switch strings.TrimSpace(stopReason) {
	case "", "end_turn", "stop":
		reason := "stop"
		return &reason
	case "tool_use":
		reason := "tool_calls"
		return &reason
	case "max_tokens":
		reason := "length"
		return &reason
	case "refusal":
		reason := "content_filter"
		return &reason
	default:
		reason := stopReason
		return &reason
	}
}

func buildOpenAINonStreamResponse(sh *streamHandler, model string, stopReason string) openAINonStreamResponse {
	textParts := make([]string, 0, len(sh.contentBlocks))
	reasoningParts := make([]string, 0, len(sh.contentBlocks))
	toolCalls := make([]openAINonStreamToolCall, 0)

	for i := range sh.contentBlocks {
		blockType, _ := sh.contentBlocks[i]["type"].(string)
		switch blockType {
		case "thinking":
			if builder := builderAt(sh.thinkingBlockBuilders, i); builder != nil {
				if reasoning := builder.String(); reasoning != "" {
					reasoningParts = append(reasoningParts, reasoning)
					continue
				}
			}
			if reasoning, ok := sh.contentBlocks[i]["thinking"].(string); ok && reasoning != "" {
				reasoningParts = append(reasoningParts, reasoning)
			}
		case "text":
			if builder := builderAt(sh.textBlockBuilders, i); builder != nil {
				if text := builder.String(); text != "" {
					textParts = append(textParts, text)
					continue
				}
			}
			if text, ok := sh.contentBlocks[i]["text"].(string); ok && text != "" {
				textParts = append(textParts, text)
			}
		case "tool_use":
			call := openAINonStreamToolCall{
				Type: "function",
			}
			if id, ok := sh.contentBlocks[i]["id"].(string); ok {
				call.ID = id
			}
			if name, ok := sh.contentBlocks[i]["name"].(string); ok {
				call.Function.Name = name
			}
			switch input := sh.contentBlocks[i]["input"].(type) {
			case string:
				call.Function.Arguments = input
			case nil:
				call.Function.Arguments = "{}"
			default:
				raw, err := json.Marshal(input)
				if err != nil {
					call.Function.Arguments = "{}"
				} else {
					call.Function.Arguments = string(raw)
				}
			}
			toolCalls = append(toolCalls, call)
		}
	}

	content := strings.Join(textParts, "")
	if strings.TrimSpace(content) == "" && len(toolCalls) > 0 {
		content = ""
	}

	message := openAINonStreamMessage{
		Role:             "assistant",
		Content:          content,
		ReasoningContent: strings.Join(reasoningParts, ""),
	}
	if len(toolCalls) > 0 {
		message.ToolCalls = toolCalls
	}

	return openAINonStreamResponse{
		ID:      sh.msgID,
		Object:  "chat.completion",
		Created: sh.startTime.Unix(),
		Model:   model,
		Choices: []openAINonStreamChoice{{
			Index:        0,
			Message:      message,
			FinishReason: mapStopReasonToOpenAIFinishReason(stopReason),
		}},
		Usage: openAINonStreamUsage{
			PromptTokens:     sh.inputTokens,
			CompletionTokens: sh.outputTokens,
			TotalTokens:      sh.inputTokens + sh.outputTokens,
		},
	}
}

// materializeBlockField copies a streamed text/thinking block's accumulated
// content into its wire field before a non-streaming response is encoded. A
// block that never streamed anything still gets the field, empty.
func materializeBlockField(block map[string]interface{}, field string, builders []*strings.Builder, idx int) {
	if builder := builderAt(builders, idx); builder != nil {
		block[field] = builder.String()
		return
	}
	if _, ok := block[field]; !ok {
		block[field] = ""
	}
}
