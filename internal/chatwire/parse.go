package chatwire

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseLooseStringAny renders an arbitrary JSON-decoded value as a trimmed
// string. A client routinely sends a number or a bool where the protocol says
// string, and a nil must read as "" rather than "<nil>", so the loose read is
// the only safe one on this boundary.
func ParseLooseStringAny(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

// ParseLooseBoolAnyForField reads a boolean that a client may send as a bool, a
// number or a word, naming the field in the error so the caller can quote it.
func ParseLooseBoolAnyForField(value any, field string) (bool, error) {
	if strings.TrimSpace(field) == "" {
		field = "value"
	}
	errText := field + " must be a boolean"
	switch v := value.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return false, nil
		}
		switch strings.ToLower(raw) {
		case "1", "true", "yes", "y", "on":
			return true, nil
		case "0", "false", "no", "n", "off":
			return false, nil
		default:
			return false, fmt.Errorf("%s", errText)
		}
	case float64:
		if v == 1 {
			return true, nil
		}
		if v == 0 {
			return false, nil
		}
		return false, fmt.Errorf("%s", errText)
	default:
		return false, fmt.Errorf("%s", errText)
	}
}

// ParseLooseBoolAny is ParseLooseBoolAnyForField for the `stream` field.
func ParseLooseBoolAny(value any) (bool, error) {
	return ParseLooseBoolAnyForField(value, "stream")
}

// ParseLooseIntAny reads an integer that a client may send as a number or a
// numeric string.
func ParseLooseIntAny(value any) (int, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int:
		return v, nil
	case int32:
		return int(v), nil
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(raw)
		if err != nil {
			return 0, err
		}
		return n, nil
	default:
		return 0, fmt.Errorf("invalid integer value")
	}
}

// ParseLooseFloatAny reads an optional float: nil stays nil, so a caller can
// tell "absent" from "zero".
func ParseLooseFloatAny(value any) (*float64, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case float64:
		out := v
		return &out, nil
	case int:
		out := float64(v)
		return &out, nil
	case int32:
		out := float64(v)
		return &out, nil
	case int64:
		out := float64(v)
		return &out, nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return nil, nil
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, err
		}
		return &n, nil
	default:
		return nil, fmt.Errorf("invalid float value")
	}
}

// ParseStringList accepts a string or an array of strings, which is how clients
// spell `stop`.
func ParseStringList(value any, field string) ([]string, error) {
	switch item := value.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{item}, nil
	case []any:
		out := make([]string, 0, len(item))
		for _, raw := range item {
			text, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a string or array of strings", field)
			}
			out = append(out, text)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s must be a string or array of strings", field)
	}
}
