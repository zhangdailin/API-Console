package util

import (
	"encoding/json"
	"fmt"
)

// CompactToolInput renders a tool-call argument value as compact JSON.
func CompactToolInput(value any) string {
	if value == nil {
		return "{}"
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

// StringValue renders an arbitrary value as a string, preferring a plain string.
func StringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
