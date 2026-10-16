package grok

import (
	"encoding/json"
	"fmt"
	"math"
)

// Copy mutable JSON containers while sharing immutable string storage. Long
// histories must not be encoded and decoded just to isolate retry mutations.
func cloneBuildPayload(payload map[string]any) (map[string]any, error) {
	if payload == nil {
		return map[string]any{}, nil
	}
	cloned, err := cloneBuildValue(payload, 0)
	if err != nil {
		return nil, err
	}
	return cloned.(map[string]any), nil
}

func cloneBuildValue(value any, depth int) (any, error) {
	if depth > 10000 {
		return nil, fmt.Errorf("request JSON is cyclic or exceeds the payload copy depth limit")
	}
	switch v := value.(type) {
	case nil, bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v, nil
	case json.Number:
		if _, err := json.Marshal(v); err != nil {
			return nil, err
		}
		return v, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("invalid JSON number")
		}
		return v, nil
	case float32:
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("invalid JSON number")
		}
		return v, nil
	case map[string]any:
		if v == nil {
			return map[string]any(nil), nil
		}
		copied := make(map[string]any, len(v))
		for k, item := range v {
			clone, err := cloneBuildValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			copied[k] = clone
		}
		return copied, nil
	case []any:
		if v == nil {
			return []any(nil), nil
		}
		copied := make([]any, len(v))
		for i, item := range v {
			clone, err := cloneBuildValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			copied[i] = clone
		}
		return copied, nil
	case []string:
		if v == nil {
			return []any(nil), nil
		}
		copied := make([]any, len(v))
		for i, item := range v {
			copied[i] = item
		}
		return copied, nil
	case []map[string]any:
		if v == nil {
			return []any(nil), nil
		}
		copied := make([]any, len(v))
		for i, item := range v {
			clone, err := cloneBuildValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			copied[i] = clone
		}
		return copied, nil
	default:
		// Preserve support for caller-supplied structs, RawMessage and custom JSON
		// types; decoded API histories take the allocation-light cases above.
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var copied any
		if err = json.Unmarshal(raw, &copied); err != nil {
			return nil, err
		}
		return copied, nil
	}
}
