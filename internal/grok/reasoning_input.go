package grok

import (
	"strings"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
)

// cleanReasoningValue removes null/output-only metadata without rewriting text
// or opaque proofs. Never apply it to tool arguments or arbitrary client data.
func cleanReasoningValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, child := range value {
			if child != nil && key != "phase" && key != "internal_chat_message_metadata_passthrough" {
				out[key] = cleanReasoningValue(child)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(value))
		for _, child := range value {
			if child != nil {
				out = append(out, cleanReasoningValue(child))
			}
		}
		return out
	default:
		return value
	}
}

func portableReasoningSummary(item map[string]any) map[string]any {
	var texts []string
	for _, field := range []string{"summary", "content"} {
		for _, part := range responses.InterfaceMaps(item[field]) {
			if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
				texts = append(texts, text)
			}
		}
	}
	if len(texts) == 0 {
		return nil
	}
	return map[string]any{"type": "message", "role": "assistant", "content": "Prior model reasoning summary:\n" + strings.Join(texts, "\n")}
}

func normalizeBuildReasoningInput(payload map[string]any) {
	input, ok := payload["input"].([]any)
	if !ok {
		return
	}
	next := make([]any, 0, len(input))
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		if item == nil || item["type"] != "reasoning" {
			next = append(next, raw)
			continue
		}
		if chatwire.ParseLooseStringAny(item["encrypted_content"]) == "" {
			if summary := portableReasoningSummary(item); summary != nil {
				next = append(next, summary)
			}
			continue
		}
		cleaned := map[string]any{"type": "reasoning"}
		for _, field := range []string{"id", "summary", "content", "encrypted_content"} {
			if value := item[field]; value != nil {
				cleaned[field] = cleanReasoningValue(value)
			}
		}
		if _, exists := cleaned["summary"]; !exists {
			cleaned["summary"] = []any{}
		}
		next = append(next, cleaned)
	}
	payload["input"] = next
}
