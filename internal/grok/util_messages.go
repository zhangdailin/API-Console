package grok

import (
	"encoding/base64"
	"fmt"
	"strings"
)

func validateChatMessages(messages []ChatMessage) error {
	for _, msg := range messages {
		roleRaw := strings.TrimSpace(msg.Role)
		role := strings.ToLower(roleRaw)
		if _, ok := allowedMessageRoles[role]; !ok {
			return fmt.Errorf("role must be one of [assistant developer system tool user]")
		}
		if role == "assistant" && len(msg.ToolCalls) > 0 {
			for _, tc := range msg.ToolCalls {
				if strings.TrimSpace(fmt.Sprint(tc.Function["name"])) == "" {
					return fmt.Errorf("assistant tool_calls.function.name cannot be empty")
				}
			}
		}
		if role == "tool" && strings.TrimSpace(msg.ToolCallID) == "" {
			return fmt.Errorf("tool messages must include tool_call_id")
		}
		switch content := msg.Content.(type) {
		case string:
			if strings.TrimSpace(content) == "" && role != "tool" && !(role == "assistant" && len(msg.ToolCalls) > 0) {
				return fmt.Errorf("message content cannot be empty")
			}
		case []interface{}:
			if len(content) == 0 {
				return fmt.Errorf("message content cannot be an empty array")
			}
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					return fmt.Errorf("content block must be an object")
				}
				if len(m) == 0 {
					return fmt.Errorf("content block cannot be empty")
				}
				rawType, hasType := m["type"]
				if !hasType {
					return fmt.Errorf("content block must have a 'type' field")
				}
				blockTypeRaw := strings.TrimSpace(fmt.Sprint(rawType))
				blockType := strings.ToLower(blockTypeRaw)
				if blockType == "" {
					return fmt.Errorf("content block 'type' cannot be empty")
				}

				if role == "user" {
					if _, ok := userContentTypes[blockType]; !ok {
						return fmt.Errorf("invalid content block type: '%s'", blockTypeRaw)
					}
				} else if blockType != "text" && !isToolResultContentType(role, blockType) {
					return fmt.Errorf("the '%s' role only supports 'text' type, got '%s'", role, blockTypeRaw)
				}

				switch blockType {
				case "text":
					text, _ := m["text"].(string)
					if strings.TrimSpace(text) == "" {
						return fmt.Errorf("text content cannot be empty")
					}
				case "image_url":
					imageURL, _ := m["image_url"].(map[string]interface{})
					if imageURL == nil {
						return fmt.Errorf("image_url must have a 'url' field")
					}
					urlVal, _ := imageURL["url"].(string)
					if err := validateMediaInput(urlVal, "image_url.url"); err != nil {
						return err
					}
				case "input_audio":
					audio, _ := m["input_audio"].(map[string]interface{})
					if audio == nil {
						return fmt.Errorf("input_audio must have a 'data' field")
					}
					dataVal, _ := audio["data"].(string)
					if err := validateMediaInput(dataVal, "input_audio.data"); err != nil {
						return err
					}
				case "file":
					fileData, _ := m["file"].(map[string]interface{})
					if fileData == nil {
						return fmt.Errorf("file must have a 'file_data' field")
					}
					dataVal, _ := fileData["file_data"].(string)
					if err := validateMediaInput(dataVal, "file.file_data"); err != nil {
						return err
					}
				case "input_text":
					text, _ := m["text"].(string)
					if strings.TrimSpace(text) == "" {
						return fmt.Errorf("input_text content cannot be empty")
					}
				case "input_image":
					// The Anthropic front end writes the image URL under
					// image_url; accept both spellings of the same wire part.
					urlVal := parseLooseStringAny(m["image_url"])
					if urlVal == "" {
						if nested, ok := m["image_url"].(map[string]interface{}); ok {
							urlVal = parseLooseStringAny(nested["url"])
						}
					}
					if urlVal == "" {
						return fmt.Errorf("input_image must have an 'image_url' field")
					}
					if err := validateMediaInput(urlVal, "input_image.image_url"); err != nil {
						return err
					}
				case "input_file":
					dataVal := parseLooseStringAny(m["file_data"])
					if dataVal == "" {
						dataVal = parseLooseStringAny(m["file_url"])
					}
					if dataVal == "" {
						return fmt.Errorf("input_file must have a 'file_data' or 'file_url' field")
					}
					if err := validateMediaInput(dataVal, "input_file"); err != nil {
						return err
					}
				}

			}
		default:
			if role == "assistant" && len(msg.ToolCalls) > 0 && msg.Content == nil {
				continue
			}
			return fmt.Errorf("message content must be a string or array")
		}
	}
	return nil
}

func validateMediaInput(value string, fieldName string) error {
	val := strings.TrimSpace(value)
	if val == "" {
		return fmt.Errorf("%s cannot be empty", fieldName)
	}
	lower := strings.ToLower(val)
	if strings.HasPrefix(lower, "data:") {
		return nil
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return nil
	}
	if looksLikeBase64Payload(val) {
		return fmt.Errorf("%s base64 must be provided as a data URI (data:<mime>;base64,...)", fieldName)
	}
	return fmt.Errorf("%s must be a URL or data URI", fieldName)
}

func looksLikeBase64Payload(value string) bool {
	candidate := strings.Join(strings.Fields(value), "")
	if len(candidate) < 32 || len(candidate)%4 != 0 {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(candidate)
	return err == nil
}

// isToolResultContentType reports whether a non-text block type is allowed on a
// non-user role. `tool` and `assistant` messages carry tool results and model
// output, and the Anthropic front end lowers those to Responses-shaped parts
// (input_text/input_image/input_file) plus the native image_url form. Rejecting
// them made an ordinary Claude Code tool_result array a hard 400.
func isToolResultContentType(role, blockType string) bool {
	if role != "tool" && role != "assistant" {
		return false
	}
	switch blockType {
	case "image_url", "input_text", "input_image", "input_file":
		return true
	default:
		return false
	}
}
