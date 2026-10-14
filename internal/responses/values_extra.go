package responses

import (
	"encoding/json"
	"strconv"
	"strings"
)

// InterfaceToInt reads a loosely-typed JSON number as an int, which is how
// usage counters arrive after a round trip through interface{}.
func InterfaceToInt(value any) int {
	switch x := value.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return i
		}
	}
	return 0
}

// FirstNonNil returns the first non-nil value, so a caller can pick whichever
// spelling of a field the upstream sent.
func FirstNonNil(values ...any) any {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}
