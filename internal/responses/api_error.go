package responses

import (
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/util"
)

// DefaultStoredResponseTTL is how long a stored response survives when the
// deployment has not configured one.
const DefaultStoredResponseTTL = 30 * 24 * time.Hour

func WriteAPIError(w http.ResponseWriter, status int, code, message string) {
	WriteAPIErrorWithParam(w, status, code, message, "")
}

// writeResponsesAPIErrorWithParam is the same envelope with a `param` that names
// the offending request field. A blob that cannot be decoded has to say which
// input item to drop, and "param" is where an OpenAI-shaped client looks.
func WriteAPIErrorWithParam(w http.ResponseWriter, status int, code, message, param string) {
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
	util.WriteJSONStatus(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"code":    code,
			"param":   paramValue,
		},
	})
}
