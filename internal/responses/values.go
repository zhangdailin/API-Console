package responses

import "maps"

// InterfaceMaps reads a JSON array as a slice of maps, accepting both the
// decoded []interface{} form and an already-typed slice.
func InterfaceMaps(value any) []map[string]any {
	switch values := value.(type) {
	case []map[string]any:
		return values
	case []any:
		out := make([]map[string]any, 0, len(values))
		for _, raw := range values {
			if typed, ok := raw.(map[string]any); ok {
				out = append(out, typed)
			}
		}
		return out
	}
	return nil
}

// InterfaceSlice reads a JSON array as a []interface{}.
func InterfaceSlice(value any) []any {
	if typed, ok := value.([]any); ok {
		return typed
	}
	return nil
}

// CloneStringInterfaceMap copies a decoded JSON object shallowly: the rewrite
// paths mutate what they were given, and a caller's payload must not change
// under it.
func CloneStringInterfaceMap(value map[string]any) map[string]any {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]any, len(value))
	maps.Copy(out, value)
	return out
}

// StreamString is the strict read: only a real string is a string. Used where
// an upstream text must be passed through byte-for-byte, never formatted.
func StreamString(value any) string {
	text, _ := value.(string)
	return text
}
