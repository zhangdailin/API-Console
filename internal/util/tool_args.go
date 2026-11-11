package util

import (
	"maps"
	"strings"

	"encoding/json"
)

// NormalizeToolDefinitions renders the OpenAI `{"type":"function","function":{
// "name","description","parameters"}}` envelope from either that shape or the
// Anthropic `{"name", "input_schema", "description"}` shape. Anything that is
// not a JSON object is dropped, as is a declaration with no name.
//
// It lives here because the WorkBuddy and Cline request builders both need the
// same envelope and each used to carry its own copy. It always
// returns a non-nil slice: callers differ only in what an empty result means,
// and each keeps that decision to itself.
//
// keepEmptyDescription sends `"description": ""` for an Anthropic declaration
// that has none, which is what WorkBuddy has always been sent; Cline omits it.
//
// The input is never modified, but its nested values are shared with the
// result: a caller that rewrites a declaration must copy it first.
func NormalizeToolDefinitions(tools []any, keepEmptyDescription bool) []any {
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		// A declaration that is already a decoded object is read in place; any
		// other value is decoded through JSON.
		decoded, ok := tool.(map[string]any)
		if !ok {
			raw, err := json.Marshal(tool)
			if err != nil || json.Unmarshal(raw, &decoded) != nil {
				continue
			}
		}
		if fn, ok := decoded["function"].(map[string]any); ok {
			if strings.TrimSpace(StringValue(fn["name"])) == "" {
				continue
			}
			decoded = maps.Clone(decoded)
			decoded["type"] = "function"
			out = append(out, decoded)
			continue
		}
		name := strings.TrimSpace(StringValue(decoded["name"]))
		if name == "" {
			continue
		}
		parameters := decoded["input_schema"]
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		function := map[string]any{"name": name, "parameters": parameters}
		if description := StringValue(decoded["description"]); keepEmptyDescription || strings.TrimSpace(description) != "" {
			function["description"] = description
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}
