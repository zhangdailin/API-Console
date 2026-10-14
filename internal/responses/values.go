package responses

import "maps"

import "orchids-api/internal/chatwire"

// ParseLooseStringAny renders an arbitrary JSON-decoded value as a trimmed
// string. A Responses payload routinely carries a number or a bool where the
// protocol says string, and a nil must read as "" rather than "<nil>", so the
// loose read is the only safe one on this boundary.
func ParseLooseStringAny(value interface{}) string {
	return chatwire.ParseLooseStringAny(value)
}

// ParseLooseBoolAnyForField uses the shared scalar parser and names the field
// in validation errors.
func ParseLooseBoolAnyForField(value interface{}, field string) (bool, error) {
	return chatwire.ParseLooseBoolAnyForField(value, field)
}

// ParseLooseBoolAny reads the stream field through the shared scalar parser.
func ParseLooseBoolAny(value interface{}) (bool, error) {
	return chatwire.ParseLooseBoolAny(value)
}

// ParseLooseIntAny reads an integer through the shared scalar parser.
func ParseLooseIntAny(value interface{}) (int, error) {
	return chatwire.ParseLooseIntAny(value)
}

// ParseLooseFloatAny reads an optional float through the shared scalar parser.
func ParseLooseFloatAny(value interface{}) (*float64, error) {
	return chatwire.ParseLooseFloatAny(value)
}

// InterfaceMaps reads a JSON array as a slice of maps, accepting both the
// decoded []interface{} form and an already-typed slice.
func InterfaceMaps(value interface{}) []map[string]interface{} {
	switch values := value.(type) {
	case []map[string]interface{}:
		return values
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(values))
		for _, raw := range values {
			if typed, ok := raw.(map[string]interface{}); ok {
				out = append(out, typed)
			}
		}
		return out
	}
	return nil
}

// InterfaceSlice reads a JSON array as a []interface{}.
func InterfaceSlice(value interface{}) []interface{} {
	if typed, ok := value.([]interface{}); ok {
		return typed
	}
	return nil
}

// CloneStringInterfaceMap copies a decoded JSON object shallowly: the rewrite
// paths mutate what they were given, and a caller's payload must not change
// under it.
func CloneStringInterfaceMap(value map[string]interface{}) map[string]interface{} {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(value))
	maps.Copy(out, value)
	return out
}

// StreamString is the strict read: only a real string is a string. Used where
// an upstream text must be passed through byte-for-byte, never formatted.
func StreamString(value interface{}) string {
	text, _ := value.(string)
	return text
}
