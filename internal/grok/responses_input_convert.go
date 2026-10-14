package grok

import (
	"fmt"
	"maps"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
	"strings"

	"encoding/json"
)

func responsesInputFromChatMessages(messages []chatwire.Message) ([]any, string) {
	items := make([]any, 0, len(messages))
	var instructions strings.Builder
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "system" || role == "developer" {
			text := strings.TrimSpace(chatMessageContentText(message.Content))
			if text != "" {
				if instructions.Len() > 0 {
					instructions.WriteString("\n\n")
				}
				instructions.WriteString(text)
			}
			continue
		}
		if role == "tool" {
			// Only tool_call_id may name the call it answers. Falling back to
			// the function name produced a function_call_output for a call id
			// that does not exist, and the upstream then rejected the whole
			// turn with a message that named neither field.
			callID := strings.TrimSpace(message.ToolCallID)
			if callID == "" {
				// A tool message without an id cannot be paired with its call;
				// skipping it keeps the rest of the turn valid instead of
				// emitting a function_call_output for an id that does not exist.
				continue
			}
			items = append(items, map[string]any{"type": "function_call_output", "call_id": callID, "output": responsesToolOutput(message.Content)})
			continue
		}
		if role == "assistant" && (strings.TrimSpace(message.ReasoningContent) != "" || strings.TrimSpace(message.ReasoningEncryptedContent) != "") {
			reasoning := map[string]any{"type": "reasoning", "summary": []any{}}
			if text := strings.TrimSpace(message.ReasoningContent); text != "" {
				reasoning["summary"] = []any{map[string]any{"type": "summary_text", "text": text}}
			}
			if encrypted := strings.TrimSpace(message.ReasoningEncryptedContent); encrypted != "" {
				reasoning["encrypted_content"] = encrypted
			}
			items = append(items, reasoning)
		}
		if role == "assistant" {
			for _, call := range message.ToolCalls {
				name := strings.TrimSpace(fmt.Sprint(call.Function["name"]))
				if name == "" {
					continue
				}
				items = append(items, map[string]any{
					"type": "function_call", "call_id": util.FirstNonEmpty(strings.TrimSpace(call.ID), "call_"+util.RandomHex(12)),
					"name": name, "arguments": stringifyToolArguments(call.Function["arguments"]),
				})
			}
		}
		parts := responsesMessageParts(message.Content, role == "assistant")
		if len(parts) == 0 {
			continue
		}
		if role != "assistant" {
			role = "user"
		}
		items = append(items, map[string]any{"type": "message", "role": role, "content": parts})
	}
	return items, strings.TrimSpace(instructions.String())
}

func validateNativeChatContent(messages []chatwire.Message) error {
	for _, message := range messages {
		for _, part := range responses.InterfaceMaps(message.Content) {
			switch chatwire.ParseLooseStringAny(part["type"]) {
			case "text", "input_text", "output_text", "image_url", "input_image":
			default:
				return fmt.Errorf("Build/Build Chat does not support content.type=%q", part["type"])
			}
		}
	}
	return nil
}

func responsesMessageParts(content any, assistant bool) []any {
	// History parts always use input_text: the upstream `input` contract only
	// guarantees input_text, and an assistant turn resent as output_text was
	// rejected (all input-side text is rewritten this way).
	textType := "input_text"
	_ = assistant
	switch value := content.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []any{map[string]any{"type": textType, "text": value}}
	case []any:
		parts := make([]any, 0, len(value))
		for _, raw := range value {
			block, _ := raw.(map[string]any)
			kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(block["type"])))
			switch kind {
			case "text", "input_text", "output_text":
				if text := fmt.Sprint(block["text"]); text != "" && text != "<nil>" {
					parts = append(parts, map[string]any{"type": textType, "text": text})
				}
			case "image_url", "input_image", "image":
				if url := responseImageURL(block); url != "" {
					part := map[string]any{"type": "input_image", "image_url": url}
					detail := chatwire.ParseLooseStringAny(block["detail"])
					if nested, ok := block["image_url"].(map[string]any); ok && detail == "" {
						detail = chatwire.ParseLooseStringAny(nested["detail"])
					}
					if detail == "" {
						// The upstream treats an absent detail as its own default,
						// which is not "auto"; stating it makes the request
						// deterministic instead of upstream-dependent.
						detail = "auto"
					}
					part["detail"] = detail
					parts = append(parts, part)
				}
			case "file_url", "input_file":
				part := map[string]any{"type": "input_file"}
				for _, key := range []string{"file_url", "file_data", "file_id", "filename"} {
					if v, ok := block[key]; ok {
						if nested, nestedOK := v.(map[string]any); nestedOK {
							v = nested["url"]
						}
						if text := strings.TrimSpace(fmt.Sprint(v)); text != "" && text != "<nil>" {
							part[key] = v
						}
					}
				}
				if len(part) > 1 {
					parts = append(parts, part)
				}
			}
		}
		return parts
	default:
		return nil
	}
}

func responsesToolOutput(content any) any {
	if text, ok := content.(string); ok {
		return text
	}
	if parts := responsesMessageParts(content, false); len(parts) > 0 {
		return parts
	}
	return chatMessageContentText(content)
}

func responseImageURL(block map[string]any) string {
	for _, key := range []string{"image_url", "url"} {
		value := block[key]
		if nested, ok := value.(map[string]any); ok {
			value = nested["url"]
		}
		if text := strings.TrimSpace(fmt.Sprint(value)); text != "" && text != "<nil>" {
			return text
		}
	}
	return ""
}

func stringifyToolArguments(value any) string {
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

func normalizeChatResponseFormat(format map[string]any) map[string]any {
	copy := responses.CloneStringInterfaceMap(format)
	if strings.EqualFold(strings.TrimSpace(fmt.Sprint(copy["type"])), "json_schema") {
		if nested, ok := copy["json_schema"].(map[string]any); ok {
			flattened := map[string]any{"type": "json_schema"}
			maps.Copy(flattened, nested)
			copy = flattened
		}
	}
	return copy
}
