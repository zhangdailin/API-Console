package util

import (
	"strings"

	"encoding/json"
)

// NormalizeToolDefinitions renders the OpenAI `{"type":"function","function":{
// "name","description","parameters"}}` envelope from either that shape or the
// Anthropic `{"name", "input_schema", "description"}` shape. Anything that is
// not a JSON object is dropped, as is a declaration with no name.
//
// It lives here because the Qoder, WorkBuddy and Cline request builders all
// need the same envelope and each used to carry their own copy. It always
// returns a non-nil slice: callers differ only in what an empty result means,
// and each keeps that decision to itself.
func NormalizeToolDefinitions(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, tool := range tools {
		raw, err := json.Marshal(tool)
		if err != nil {
			continue
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			continue
		}
		if fn, ok := decoded["function"].(map[string]interface{}); ok {
			if strings.TrimSpace(StringValue(fn["name"])) == "" {
				continue
			}
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
			parameters = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": StringValue(decoded["description"]),
				"parameters":  parameters,
			},
		})
	}
	return out
}
