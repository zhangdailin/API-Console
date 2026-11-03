package web

import (
	"encoding/base64"
	"fmt"
	"strings"
)

type AttachmentInput struct {
	Type string
	Data string
}

func extractMessageAndAttachmentsWithTools(messages []ChatMessage, isVideo bool, tools []ToolDef, toolChoice interface{}, parallelToolCalls bool) (string, []AttachmentInput, error) {
	if len(tools) > 0 {
		messages = formatToolHistory(messages)
	}
	flatten := make([]struct {
		Role string
		Text string
	}, 0, len(messages))
	attachments := make([]AttachmentInput, 0)

	for _, msg := range messages {
		role := normalizeMessageRole(msg.Role)
		switch content := msg.Content.(type) {
		case string:
			text := strings.TrimSpace(content)
			if text != "" {
				flatten = append(flatten, struct {
					Role string
					Text string
				}{Role: role, Text: text})
			}
		case []interface{}:
			var parts []string
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				switch blockType {
				case "text":
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						parts = append(parts, strings.TrimSpace(s))
					}
				case "image_url":
					if data := extractAttachmentURL(m["image_url"]); data != "" {
						attachments = append(attachments, AttachmentInput{Type: "image", Data: data})
					}
				case "file":
					if isVideo {
						return "", nil, fmt.Errorf("video model does not support file content blocks")
					}
					if data := extractAttachmentURL(m["file"]); data != "" {
						attachments = append(attachments, AttachmentInput{Type: "file", Data: data})
					}
				case "input_audio":
					if isVideo {
						return "", nil, fmt.Errorf("video model does not support input_audio content blocks")
					}
					if data := extractAttachmentData(m["input_audio"], "data"); data != "" {
						attachments = append(attachments, AttachmentInput{Type: "audio", Data: data})
					}
				}
			}
			text := strings.TrimSpace(strings.Join(parts, "\n"))
			if text != "" {
				flatten = append(flatten, struct {
					Role string
					Text string
				}{Role: role, Text: text})
			}
		default:
			text := strings.TrimSpace(extractContentText(content))
			if text != "" {
				flatten = append(flatten, struct {
					Role string
					Text string
				}{Role: role, Text: text})
			}
		}
	}

	lastUser := -1
	for i := len(flatten) - 1; i >= 0; i-- {
		if flatten[i].Role == "user" {
			lastUser = i
			break
		}
	}
	if len(flatten) == 0 {
		return "", attachments, nil
	}

	var parts []string
	for i, item := range flatten {
		if i == lastUser {
			parts = append(parts, item.Text)
			continue
		}
		role := item.Role
		if role == "" {
			role = "user"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", role, item.Text))
	}
	combined := strings.Join(parts, "\n\n")
	if strings.TrimSpace(combined) == "" && len(attachments) > 0 {
		combined = "Refer to the following content:"
	}
	if prompt := buildToolPrompt(tools, toolChoice, parallelToolCalls); prompt != "" {
		if strings.TrimSpace(combined) != "" {
			combined = prompt + "\n\n" + combined
		} else {
			combined = prompt
		}
	}
	return combined, attachments, nil
}

func extractLastUserText(messages []ChatMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if normalizeMessageRole(msg.Role) != "user" {
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			return strings.TrimSpace(content)
		case []interface{}:
			parts := make([]string, 0, len(content))
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				if strings.EqualFold(fmt.Sprint(m["type"]), "text") {
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						parts = append(parts, strings.TrimSpace(s))
					}
				}
			}
			return strings.TrimSpace(strings.Join(parts, "\n"))
		default:
			return strings.TrimSpace(extractContentText(content))
		}
	}
	return ""
}

func extractContentText(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]interface{})
			if !ok {
				continue
			}
			if strings.EqualFold(fmt.Sprint(m["type"]), "text") {
				if s, ok := m["text"].(string); ok {
					s = strings.TrimSpace(s)
					if s != "" {
						parts = append(parts, s)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func normalizeMessageRole(role string) string {
	role = strings.ToLower(strings.TrimSpace(role))
	switch role {
	case "function":
		return "tool"
	case "":
		return "user"
	default:
		return role
	}
}

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

func extractAttachmentURL(v interface{}) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]interface{}:
		if u, ok := x["url"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
		if u, ok := x["data"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
		if u, ok := x["file_data"].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

func extractAttachmentData(v interface{}, field string) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]interface{}:
		if u, ok := x[field].(string); ok && strings.TrimSpace(u) != "" {
			return strings.TrimSpace(u)
		}
	}
	return ""
}

func extractPromptAndImageURLs(messages []ChatMessage) (string, []string) {
	prompt := ""
	imageURLs := make([]string, 0)

	for _, msg := range messages {
		role := strings.TrimSpace(msg.Role)
		switch content := msg.Content.(type) {
		case string:
			text := strings.TrimSpace(content)
			if text != "" {
				prompt = text
			}
		case []interface{}:
			for _, block := range content {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				switch blockType {
				case "text":
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						prompt = strings.TrimSpace(s)
					}
				case "image_url":
					if role != "user" {
						continue
					}
					if data := extractAttachmentURL(m["image_url"]); data != "" {
						imageURLs = append(imageURLs, data)
					}
				}
			}
		default:
			text := strings.TrimSpace(extractContentText(content))
			if text != "" {
				prompt = text
			}
		}
	}
	return prompt, imageURLs
}

func extractVideoPromptAndAttachments(messages []ChatMessage) (string, []AttachmentInput, error) {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		content := msg.Content
		switch c := content.(type) {
		case string:
			if text := strings.TrimSpace(c); text != "" {
				return text, nil, nil
			}
		case []interface{}:
			textParts := make([]string, 0)
			refs := make([]AttachmentInput, 0)
			for _, block := range c {
				m, ok := block.(map[string]interface{})
				if !ok {
					continue
				}
				blockType := strings.ToLower(strings.TrimSpace(fmt.Sprint(m["type"])))
				switch blockType {
				case "text":
					if s, ok := m["text"].(string); ok && strings.TrimSpace(s) != "" {
						textParts = append(textParts, strings.TrimSpace(s))
					}
				case "image_url":
					if data := extractAttachmentURL(m["image_url"]); data != "" {
						refs = append(refs, AttachmentInput{Type: "image", Data: data})
					}
				case "file":
					return "", nil, fmt.Errorf("video model does not support file content blocks")
				case "input_audio":
					return "", nil, fmt.Errorf("video model does not support input_audio content blocks")
				}
			}
			if len(textParts) > 0 {
				if len(refs) > 7 {
					refs = refs[:7]
				}
				return strings.Join(textParts, " "), refs, nil
			}
		default:
			if text := strings.TrimSpace(extractContentText(content)); text != "" {
				return text, nil, nil
			}
		}
	}
	return "", nil, fmt.Errorf("video prompt cannot be empty")
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
