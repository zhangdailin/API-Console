package toolname

import "strings"

// ExtractToolSpecFields reads the common OpenAI/Anthropic tool definition shapes.
func ExtractToolSpecFields(tool any) (string, string, map[string]any) {
	tm, ok := tool.(map[string]any)
	if !ok {
		return "", "", nil
	}

	var name string
	var description string
	var schema map[string]any

	if fn, ok := tm["function"].(map[string]any); ok {
		if v, ok := fn["name"].(string); ok {
			name = strings.TrimSpace(v)
		}
		if v, ok := fn["description"].(string); ok {
			description = v
		}
		schema = extractSchemaMap(fn, "parameters")
	}
	if name == "" {
		if v, ok := tm["name"].(string); ok {
			name = strings.TrimSpace(v)
		}
	}
	if description == "" {
		if v, ok := tm["description"].(string); ok {
			description = v
		}
	}
	if schema == nil {
		schema = extractSchemaMap(tm, "input_schema", "parameters")
	}
	return name, description, schema
}

func extractSchemaMap(tm map[string]any, keys ...string) map[string]any {
	if tm == nil {
		return nil
	}
	for _, key := range keys {
		if v, ok := tm[key]; ok {
			if schema, ok := v.(map[string]any); ok {
				return schema
			}
		}
	}
	return nil
}
