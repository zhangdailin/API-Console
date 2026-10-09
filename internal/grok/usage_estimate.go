package grok

import (
	"fmt"
	"orchids-api/internal/chatwire"
	"strings"
	"unicode/utf8"

	"encoding/json"
)

const (
	estimatedImagePromptTokens = 256
	estimatedAudioPromptTokens = 128
)

type chatUsageEstimate struct {
	promptTextTokens     int
	promptAudioTokens    int
	promptImageTokens    int
	completionTextTokens int
}

func approxTokenCount(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	if runes <= 0 {
		return 0
	}
	return max(1, (runes+3)/4)
}

func estimatePromptUsageFromRequest(req *chatwire.Request) chatUsageEstimate {
	var out chatUsageEstimate
	if req == nil {
		return out
	}
	for _, msg := range req.Messages {
		out.promptTextTokens += approxTokenCount(msg.Role)
		out.promptTextTokens += approxTokenCount(msg.Name)
		out.promptTextTokens += approxTokenCount(msg.ToolCallID)
		accumulatePromptContentUsage(&out, msg.Content)
		for _, tc := range msg.ToolCalls {
			out.promptTextTokens += approxTokenCount(tc.ID)
			out.promptTextTokens += approxTokenCount(tc.Type)
			if len(tc.Function) > 0 {
				if raw, err := json.Marshal(tc.Function); err == nil {
					out.promptTextTokens += approxTokenCount(string(raw))
				}
			}
		}
	}
	if len(req.Tools) > 0 {
		if raw, err := json.Marshal(req.Tools); err == nil {
			out.promptTextTokens += approxTokenCount(string(raw))
		}
	}
	if req.ToolChoice != nil {
		if raw, err := json.Marshal(req.ToolChoice); err == nil {
			out.promptTextTokens += approxTokenCount(string(raw))
		}
	}
	return out
}

func accumulatePromptContentUsage(out *chatUsageEstimate, content interface{}) {
	if out == nil || content == nil {
		return
	}
	switch v := content.(type) {
	case string:
		out.promptTextTokens += approxTokenCount(v)
	case []interface{}:
		for _, block := range v {
			accumulatePromptContentUsage(out, block)
		}
	case map[string]interface{}:
		blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(v["type"])))
		switch blockType {
		case "text", "input_text":
			out.promptTextTokens += approxTokenCount(chatwire.ParseLooseStringAny(v["text"]))
		case "image_url":
			out.promptImageTokens += estimatedImagePromptTokens
			if imageURL, ok := v["image_url"].(map[string]interface{}); ok {
				out.promptTextTokens += approxTokenCount(chatwire.ParseLooseStringAny(imageURL["detail"]))
			}
		case "input_audio":
			out.promptAudioTokens += estimatedAudioPromptTokens
		case "file":
			// A file block is a document, not an image: charging it 256 image
			// tokens invented usage the upstream never reported.
			if fileData, ok := v["file"].(map[string]interface{}); ok {
				out.promptTextTokens += approxTokenCount(chatwire.ParseLooseStringAny(fileData["filename"]))
			}
		default:
			if raw, err := json.Marshal(v); err == nil {
				out.promptTextTokens += approxTokenCount(string(raw))
			}
		}
	default:
		out.promptTextTokens += approxTokenCount(fmt.Sprint(v))
	}
}

func estimateCompletionUsage(finalContent string, toolCalls []map[string]interface{}) chatUsageEstimate {
	var out chatUsageEstimate
	out.completionTextTokens += approxTokenCount(finalContent)
	// Count the generated payload, not its JSON envelope: key names, quotes and
	// commas are serialization overhead, not tokens the model produced.
	for _, call := range toolCalls {
		function, _ := call["function"].(map[string]interface{})
		out.completionTextTokens += approxTokenCount(chatwire.ParseLooseStringAny(function["name"]))
		out.completionTextTokens += approxTokenCount(stringifyToolArguments(function["arguments"]))
	}
	return out
}

func buildChatUsagePayload(req *chatwire.Request, finalContent string, toolCalls []map[string]interface{}) map[string]interface{} {
	prompt := estimatePromptUsageFromRequest(req)
	completion := estimateCompletionUsage(finalContent, toolCalls)
	promptTokens := prompt.promptTextTokens + prompt.promptAudioTokens + prompt.promptImageTokens
	completionTokens := completion.completionTextTokens
	return map[string]interface{}{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens": 0,
			"text_tokens":   prompt.promptTextTokens,
			"audio_tokens":  prompt.promptAudioTokens,
			"image_tokens":  prompt.promptImageTokens,
		},
		"completion_tokens_details": map[string]interface{}{
			"text_tokens": completion.completionTextTokens,
			// Audio/reasoning estimates are not tracked; kept as zero for API parity.
			"audio_tokens":     0,
			"reasoning_tokens": 0,
		},
	}
}

func addReasoningUsage(usage map[string]interface{}, reasoning string) map[string]interface{} {
	if usage == nil || strings.TrimSpace(reasoning) == "" {
		return usage
	}
	reasoningTokens := approxTokenCount(reasoning)
	if reasoningTokens <= 0 {
		return usage
	}
	details, _ := usage["completion_tokens_details"].(map[string]interface{})
	if details == nil {
		details = map[string]interface{}{}
		usage["completion_tokens_details"] = details
	}
	details["reasoning_tokens"] = interfaceToInt(details["reasoning_tokens"]) + reasoningTokens
	usage["completion_tokens"] = interfaceToInt(usage["completion_tokens"]) + reasoningTokens
	usage["total_tokens"] = interfaceToInt(usage["total_tokens"]) + reasoningTokens
	return usage
}
