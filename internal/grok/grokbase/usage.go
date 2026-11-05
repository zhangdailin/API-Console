package grokbase

import (
	"fmt"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"unicode/utf8"

	"encoding/json"
)

const (
	estimatedImagePromptTokens = 256
	estimatedAudioPromptTokens = 128
)

// UsageEstimate is a local token estimate for one chat turn.
type UsageEstimate struct {
	PromptTextTokens     int
	PromptAudioTokens    int
	PromptImageTokens    int
	CompletionTextTokens int
}

// ApproxTokenCount estimates tokens as one per four runes; the upstream does
// not report usage on every path, and this keeps the estimate comparable.
func ApproxTokenCount(text string) int {
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

// EstimatePrompt estimates the prompt side of a chat request.
func EstimatePrompt(messages []chatwire.Message, tools []chatwire.ToolDef, toolChoice any) UsageEstimate {
	var out UsageEstimate
	for _, msg := range messages {
		out.PromptTextTokens += ApproxTokenCount(msg.Role)
		out.PromptTextTokens += ApproxTokenCount(msg.Name)
		out.PromptTextTokens += ApproxTokenCount(msg.ToolCallID)
		accumulatePromptContentUsage(&out, msg.Content)
		for _, tc := range msg.ToolCalls {
			out.PromptTextTokens += ApproxTokenCount(tc.ID)
			out.PromptTextTokens += ApproxTokenCount(tc.Type)
			if len(tc.Function) > 0 {
				if raw, err := json.Marshal(tc.Function); err == nil {
					out.PromptTextTokens += ApproxTokenCount(string(raw))
				}
			}
		}
	}
	if len(tools) > 0 {
		if raw, err := json.Marshal(tools); err == nil {
			out.PromptTextTokens += ApproxTokenCount(string(raw))
		}
	}
	if toolChoice != nil {
		if raw, err := json.Marshal(toolChoice); err == nil {
			out.PromptTextTokens += ApproxTokenCount(string(raw))
		}
	}
	return out
}

func accumulatePromptContentUsage(out *UsageEstimate, content any) {
	if out == nil || content == nil {
		return
	}
	switch v := content.(type) {
	case string:
		out.PromptTextTokens += ApproxTokenCount(v)
	case []any:
		for _, block := range v {
			accumulatePromptContentUsage(out, block)
		}
	case map[string]any:
		blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(v["type"])))
		switch blockType {
		case "text", "input_text":
			out.PromptTextTokens += ApproxTokenCount(chatwire.ParseLooseStringAny(v["text"]))
		case "image_url":
			out.PromptImageTokens += estimatedImagePromptTokens
			if imageURL, ok := v["image_url"].(map[string]any); ok {
				out.PromptTextTokens += ApproxTokenCount(chatwire.ParseLooseStringAny(imageURL["detail"]))
			}
		case "input_audio":
			out.PromptAudioTokens += estimatedAudioPromptTokens
		case "file":
			// A file block is a document, not an image: charging it 256 image
			// tokens invented usage the upstream never reported.
			if fileData, ok := v["file"].(map[string]any); ok {
				out.PromptTextTokens += ApproxTokenCount(chatwire.ParseLooseStringAny(fileData["filename"]))
			}
		default:
			if raw, err := json.Marshal(v); err == nil {
				out.PromptTextTokens += ApproxTokenCount(string(raw))
			}
		}
	default:
		out.PromptTextTokens += ApproxTokenCount(fmt.Sprint(v))
	}
}

// EstimateCompletion estimates the completion side: text plus tool-call names
// and arguments.
func EstimateCompletion(finalContent string, toolCalls []map[string]any) UsageEstimate {
	var out UsageEstimate
	out.CompletionTextTokens += ApproxTokenCount(finalContent)
	// Count the generated payload, not its JSON envelope: key names, quotes and
	// commas are serialization overhead, not tokens the model produced.
	for _, call := range toolCalls {
		function, _ := call["function"].(map[string]any)
		out.CompletionTextTokens += ApproxTokenCount(chatwire.ParseLooseStringAny(function["name"]))
		out.CompletionTextTokens += ApproxTokenCount(StringifyToolArguments(function["arguments"]))
	}
	return out
}

// ChatUsagePayload renders an OpenAI Chat Completions usage object from a
// prompt estimate and the generated output.
func ChatUsagePayload(prompt UsageEstimate, finalContent string, toolCalls []map[string]any) map[string]any {
	completion := EstimateCompletion(finalContent, toolCalls)
	promptTokens := prompt.PromptTextTokens + prompt.PromptAudioTokens + prompt.PromptImageTokens
	completionTokens := completion.CompletionTextTokens
	return map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": completionTokens,
		"total_tokens":      promptTokens + completionTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 0,
			"text_tokens":   prompt.PromptTextTokens,
			"audio_tokens":  prompt.PromptAudioTokens,
			"image_tokens":  prompt.PromptImageTokens,
		},
		"completion_tokens_details": map[string]any{
			"text_tokens": completion.CompletionTextTokens,
			// Audio/reasoning estimates are not tracked; kept as zero for API parity.
			"audio_tokens":     0,
			"reasoning_tokens": 0,
		},
	}
}

// AddReasoningUsage adds an estimate of reasoning text to a usage object.
func AddReasoningUsage(usage map[string]any, reasoning string) map[string]any {
	if usage == nil || strings.TrimSpace(reasoning) == "" {
		return usage
	}
	reasoningTokens := ApproxTokenCount(reasoning)
	if reasoningTokens <= 0 {
		return usage
	}
	details, _ := usage["completion_tokens_details"].(map[string]any)
	if details == nil {
		details = map[string]any{}
		usage["completion_tokens_details"] = details
	}
	details["reasoning_tokens"] = responses.InterfaceToInt(details["reasoning_tokens"]) + reasoningTokens
	usage["completion_tokens"] = responses.InterfaceToInt(usage["completion_tokens"]) + reasoningTokens
	usage["total_tokens"] = responses.InterfaceToInt(usage["total_tokens"]) + reasoningTokens
	return usage
}

// StringifyToolArguments renders tool-call arguments as the JSON text the
// upstream expects: a non-empty string as is, anything else marshalled, "{}"
// for nothing.
func StringifyToolArguments(value any) string {
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text)
	}
	if value == nil {
		return "{}"
	}
	if raw, err := json.Marshal(value); err == nil {
		return string(raw)
	}
	return "{}"
}
