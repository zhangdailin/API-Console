package chatwire

import (
	"encoding/json"
	"fmt"
	"strings"
)

// NativeToolTypes are the hosted (server-side) tools an OpenAI-compatible
// client may declare in `tools`. They have no `function` object; they are
// forwarded to the upstream Responses plane as native tools.
var NativeToolTypes = map[string]string{
	"web_search":                    "web_search",
	"web_search_preview":            "web_search",
	"web_search_preview_2025_03_11": "web_search",
	"web_search_2025_08_26":         "web_search",
	"x_search":                      "x_search",
}

// ValidateToolDefinitions accepts the function and hosted tool shapes and
// rejects anything else, so a declaration the upstream cannot serve fails here
// rather than becoming a confusing upstream error.
func ValidateToolDefinitions(tools []ToolDef) error {
	return validateToolDefinitions(tools, false)
}

// ValidateStrictToolDefinitions also checks function names and parameters as
// JSON values for callers that render tool definitions themselves.
func ValidateStrictToolDefinitions(tools []ToolDef) error {
	return validateToolDefinitions(tools, true)
}

func validateToolDefinitions(tools []ToolDef, strict bool) error {
	seen := make(map[string]struct{}, len(tools))
	for i, tool := range tools {
		if normalized, native := NativeToolTypes[strings.ToLower(strings.TrimSpace(tool.Type))]; native {
			if _, exists := seen[normalized]; exists {
				return fmt.Errorf("tools.%d.type duplicates %q", i, normalized)
			}
			seen[normalized] = struct{}{}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			return fmt.Errorf("tools.%d.type must be function, web_search or x_search", i)
		}
		var name string
		if strict {
			if tool.Function == nil {
				return fmt.Errorf("tools.%d.function is required", i)
			}
			name, _ = tool.Function["name"].(string)
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("tools.%d.function.name must be a non-empty string", i)
			}
		} else {
			name = strings.TrimSpace(fmt.Sprint(tool.Function["name"]))
			if name == "" || name == "<nil>" {
				return fmt.Errorf("tools.%d.function.name is required", i)
			}
		}
		key := strings.TrimSpace(name)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("tools.%d.function.name duplicates %q", i, name)
		}
		seen[key] = struct{}{}
		if parameters := tool.Function["parameters"]; strict && parameters != nil {
			raw, err := json.Marshal(parameters)
			var decoded any
			if err != nil || json.Unmarshal(raw, &decoded) != nil {
				return fmt.Errorf("tools.%d.function.parameters must be valid JSON", i)
			}
		}
	}
	return nil
}
