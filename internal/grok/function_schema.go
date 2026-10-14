package grok

import "orchids-api/internal/responses"

// Build checks the function root before resolving schema combinators. Expose
// the object argument contract at the root, retaining every branch and sibling
// constraint. Nullable roots are restricted to objects because function-call
// arguments must be JSON objects; nullable properties remain unchanged.
func normalizeBuildFunctionSchema(schema map[string]any) map[string]any {
	out := responses.CloneStringInterfaceMap(schema)
	objectRoot := out["type"] == "object"
	if types, ok := out["type"].([]any); ok && len(types) > 0 {
		hasObject, onlyObjectOrNull := false, true
		for _, kind := range types {
			hasObject = hasObject || kind == "object"
			onlyObjectOrNull = onlyObjectOrNull && (kind == "object" || kind == "null")
		}
		objectRoot = hasObject && onlyObjectOrNull
	}
	if _, typed := out["type"]; !typed {
		for _, keyword := range []string{"anyOf", "oneOf"} {
			branches, ok := out[keyword].([]any)
			hasObject, onlyObjectOrNull := false, ok && len(branches) > 0
			for _, raw := range branches {
				branch, ok := raw.(map[string]any)
				kind := branch["type"]
				hasObject = hasObject || kind == "object"
				onlyObjectOrNull = onlyObjectOrNull && ok && (kind == "object" || kind == "null")
			}
			objectRoot = objectRoot || (hasObject && onlyObjectOrNull)
		}
	}
	if !objectRoot {
		return out
	}
	if raw, exists := out["allOf"]; exists {
		if _, ok := raw.([]any); !ok {
			return out // Do not erase an unrecognized existing constraint.
		}
	}
	out["type"] = "object"
	conjuncts := append([]any(nil), responses.InterfaceSlice(out["allOf"])...)
	for _, keyword := range []string{"anyOf", "oneOf"} {
		if branches, exists := out[keyword]; exists {
			conjuncts = append(conjuncts, map[string]any{keyword: branches})
			delete(out, keyword)
		}
	}
	if len(conjuncts) > 0 {
		out["allOf"] = conjuncts
	}
	return out
}

func normalizeBuildFunctionTools(tools []map[string]any) []map[string]any {
	tools = append([]map[string]any(nil), tools...)
	for i, tool := range tools {
		if tool["type"] != "function" {
			continue
		}
		if schema, ok := tool["parameters"].(map[string]any); ok {
			copy := responses.CloneStringInterfaceMap(tool)
			copy["parameters"] = normalizeBuildFunctionSchema(schema)
			tools[i] = copy
		}
	}
	return tools
}
