package responses

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// NormalizeAgentMessages lowers Codex delegation messages to portable user
// messages. encrypted_content is a legacy name for plaintext in these task
// blocks; reasoning and compaction proofs are never decoded or rewritten here.
// Copies are made only for changed items, preserving caller-owned history.
func NormalizeAgentMessages(input any) (any, error) {
	items, ok := input.([]any)
	if !ok {
		return input, nil
	}
	var normalized []any
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := item["type"].(string)
		explicit := kind == "agent_message"
		parts, array := item["content"].([]any)
		if !explicit {
			role, _ := item["role"].(string)
			if (kind != "message" && kind != "") || role != "user" || !array || !hasAgentEncryptedPart(parts) || !hasAgentTaskEnvelope(parts) {
				continue
			}
		}
		var content any
		if text, ok := item["content"].(string); explicit && ok && strings.TrimSpace(text) != "" {
			content = text
		} else {
			if !array || len(parts) == 0 {
				return nil, fmt.Errorf("input[%d]: agent message content must be nonempty text or an array", i)
			}
			converted := make([]any, len(parts))
			for j, raw := range parts {
				part, ok := raw.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("input[%d].content[%d]: agent content must be an object", i, j)
				}
				switch part["type"] {
				case "encrypted_content":
					text, ok := part["encrypted_content"].(string)
					if !ok || strings.TrimSpace(text) == "" {
						return nil, fmt.Errorf("input[%d].content[%d]: agent task text must be a nonempty string", i, j)
					}
					converted[j] = map[string]any{"type": "input_text", "text": text}
				case "input_text":
					text, ok := part["text"].(string)
					if !ok || strings.TrimSpace(text) == "" {
						return nil, fmt.Errorf("input[%d].content[%d]: agent input_text must be a nonempty string", i, j)
					}
					converted[j] = part
				case "input_image", "input_file":
					converted[j] = part
				default:
					return nil, fmt.Errorf("input[%d].content[%d]: unsupported agent content type", i, j)
				}
			}
			content = converted
		}
		if normalized == nil {
			normalized = slices.Clone(items)
		}
		if explicit {
			// Agent routing metadata has no meaning to the model endpoint. Keep
			// delegated instructions at user priority, even if a role was supplied.
			normalized[i] = map[string]any{"type": "message", "role": "user", "content": content}
		} else {
			copy := maps.Clone(item)
			copy["type"] = "message"
			copy["content"] = content
			normalized[i] = copy
		}
	}
	if normalized == nil {
		return input, nil
	}
	return normalized, nil
}

func hasAgentEncryptedPart(parts []any) bool {
	for _, raw := range parts {
		if part, ok := raw.(map[string]any); ok && part["type"] == "encrypted_content" {
			return true
		}
	}
	return false
}

// A nested task needs both ordered envelope fields, not an incidental mention
// of "Payload:" in ordinary text. Explicit agent_message items need no envelope.
func hasAgentTaskEnvelope(parts []any) bool {
	messageType := false
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok || part["type"] != "input_text" {
			continue
		}
		text, _ := part["text"].(string)
		for line := range strings.SplitSeq(text, "\n") {
			line = strings.TrimSpace(line)
			if value, ok := strings.CutPrefix(line, "Message Type:"); ok && strings.TrimSpace(value) != "" {
				messageType = true
			}
			if messageType && strings.HasPrefix(line, "Payload:") {
				return true
			}
		}
	}
	return false
}
