package util

import (
	"fmt"
	"strings"

	"encoding/json"

	"orchids-api/internal/prompt"
)

// StringifyToolResult renders a tool result the way every provider's message
// builder needs it: text blocks joined by newlines, a plain string as itself,
// and anything else as JSON. The providers used to carry a byte-identical copy
// of this switch each.
func StringifyToolResult(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []prompt.ContentBlock:
		parts := make([]string, 0, len(typed))
		for _, block := range typed {
			if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(raw)
	}
}
