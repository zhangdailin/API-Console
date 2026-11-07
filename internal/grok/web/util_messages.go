package web

import (
	"fmt"
	"slices"
	"strings"
)

type AttachmentInput struct {
	Type string
	Data string
}

func extractMessageAndAttachmentsWithTools(messages []ChatMessage, isVideo bool, tools []ToolDef, toolChoice any, parallelToolCalls bool) (string, []AttachmentInput, error) {
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
		case []any:
			var parts []string
			for _, block := range content {
				m, ok := block.(map[string]any)
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
	for i, f := range slices.Backward(flatten) {
		if f.Role == "user" {
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
	for _, msg := range slices.Backward(messages) {

		if normalizeMessageRole(msg.Role) != "user" {
			continue
		}
		switch content := msg.Content.(type) {
		case string:
			return strings.TrimSpace(content)
		case []any:
			parts := make([]string, 0, len(content))
			for _, block := range content {
				m, ok := block.(map[string]any)
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

func extractContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, block := range v {
			m, ok := block.(map[string]any)
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

func extractAttachmentURL(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
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

func extractAttachmentData(v any, field string) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
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
		case []any:
			for _, block := range content {
				m, ok := block.(map[string]any)
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
	for _, msg := range slices.Backward(messages) {

		content := msg.Content
		switch c := content.(type) {
		case string:
			if text := strings.TrimSpace(c); text != "" {
				return text, nil, nil
			}
		case []any:
			textParts := make([]string, 0)
			refs := make([]AttachmentInput, 0)
			for _, block := range c {
				m, ok := block.(map[string]any)
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
