package grok

import (
	"fmt"
	"strings"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/httpclient"
)

// The Build CLI proxy consumes native Responses file parts. This bridge never
// uploads files itself or resolves opaque file IDs on pooled accounts.
func normalizeBuildChatDocuments(messages []chatwire.Message) error {
	for i := range messages {
		message := &messages[i]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		parts, ok := message.Content.([]any)
		if !ok || !containsBuildChatFile(parts) {
			continue
		}
		converted := make([]any, 0, len(parts))
		for j, raw := range parts {
			part, _ := raw.(map[string]any)
			switch buildContentType(part) {
			case "file", "input_file":
				file, err := buildChatFilePart(part)
				if err != nil {
					return fmt.Errorf("messages[%d].content[%d]: %w", i, j, err)
				}
				converted = append(converted, file)
			default:
				converted = append(converted, raw)
			}
		}
		message.Content = converted
	}
	return nil
}

func buildContentType(part map[string]any) string {
	kind, _ := part["type"].(string)
	return strings.ToLower(strings.TrimSpace(kind))
}

func containsBuildChatFile(parts []any) bool {
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if kind := buildContentType(part); kind == "file" || kind == "input_file" {
			return true
		}
	}
	return false
}

func buildChatFilePart(part map[string]any) (map[string]any, error) {
	src := part
	if nested, ok := part["file"].(map[string]any); ok {
		for _, key := range []string{"file_url", "file_data", "file_id"} {
			if part[key] != nil {
				return nil, fmt.Errorf("do not mix nested and top-level file sources")
			}
		}
		src = nested
	}
	if value := src["file_id"]; value != nil {
		if id, ok := value.(string); !ok || strings.TrimSpace(id) != "" {
			return nil, fmt.Errorf("Build Chat does not support file_id; use file_url or file_data")
		}
	}
	data, _ := src["file_data"].(string)
	url, _ := src["file_url"].(string)
	data, url = strings.TrimSpace(data), strings.TrimSpace(url)
	if (data == "") == (url == "") {
		return nil, fmt.Errorf("provide exactly one of file_url or file_data")
	}
	// Older clients used file_data for a URL; emit the correct upstream field.
	if lower := strings.ToLower(data); strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "http://") {
		data, url = "", data
	}
	out := map[string]any{"type": "input_file"}
	if url != "" {
		if _, err := httpclient.ValidateRemoteFetchURL(url); err != nil {
			return nil, fmt.Errorf("file_url must be an HTTP or HTTPS URL without credentials")
		}
		out["file_url"] = url
	} else {
		header, payload, ok := strings.Cut(data, ",")
		if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") ||
			!strings.HasSuffix(strings.ToLower(header), ";base64") || strings.TrimSpace(payload) == "" {
			return nil, fmt.Errorf("file_data must be a Base64 data URI")
		}
		out["file_data"] = data
	}
	if name, ok := src["filename"].(string); ok && strings.TrimSpace(name) != "" {
		out["filename"] = strings.TrimSpace(name)
	}
	return out, nil
}
