package web

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Small value helpers shared by the Web plane handlers.

func interfaceMaps(value any) []map[string]any {
	switch values := value.(type) {
	case []map[string]any:
		return values
	case []any:
		out := make([]map[string]any, 0, len(values))
		for _, value := range values {
			if item, ok := value.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out
	default:
		return nil
	}
}

// tokenCacheKey is a non-reversible key for a credential, safe for maps and logs.
func tokenCacheKey(token string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
