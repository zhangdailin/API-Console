package web

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
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

func writeResponsesAPIError(w http.ResponseWriter, status int, code, message string) {
	writeResponsesAPIErrorWithParam(w, status, code, message, "")
}

func writeResponsesAPIErrorWithParam(w http.ResponseWriter, status int, code, message, param string) {
	// The type has to follow the status: a client retries an overload or a rate
	// limit and stops on a bad request, and a constant invalid_request_error
	// told every client to stop, including for a 503 it could have retried.
	errType := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		errType = "authentication_error"
	case status == http.StatusForbidden:
		errType = "permission_error"
	case status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case status >= 500:
		errType = "server_error"
	}
	var paramValue any
	if strings.TrimSpace(param) != "" {
		paramValue = param
	}
	writeJSONStatus(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
			"param":   paramValue,
		},
	})
}

// tokenCacheKey is a non-reversible key for a credential, safe for maps and logs.
func tokenCacheKey(token string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}
