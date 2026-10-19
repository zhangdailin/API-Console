package qoder

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

// refusalVerdict reduces an error to the retry decision the handler acts on.
func refusalVerdict(err error) string {
	var attempt *attemptStreamError
	isAttempt := errors.As(err, &attempt)
	switch {
	case err == nil:
		return "none"
	case isAttempt && attempt.busy:
		return "busy"
	case errors.Is(err, ErrDailyCountExceeded):
		return "daily"
	case strings.Contains(err.Error(), "duplicate request"):
		return "duplicate"
	}
	if _, ok := errors.AsType[*agentLimitError](err); ok {
		return "agent-limit"
	}
	switch {
	case errors.Is(err, ErrNoEntitlement):
		return "entitlement"
	case errors.Is(err, ErrContentPolicy):
		return "content-policy"
	case isAttempt && attempt.unauth:
		return "unauthorized"
	case errors.Is(err, ErrClientFault):
		return "client-fault"
	case isTransientError(err):
		return "transient"
	case isAttempt && attempt.retryable:
		return "retryable"
	}
	return "final"
}

// TestRefusalVerdictIsLayerIndependent sends each upstream refusal once as an
// HTTP status and once as a non-200 envelope inside a 200 stream. The two
// layers used to keep separate decision ladders that had drifted apart, so the
// same body could park an account on one path and not on the other.
func TestRefusalVerdictIsLayerIndependent(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"queue refusal under 401", 401, `{"code":"10605","message":"queue full"}`, "busy"},
		{"daily count", 403, `{"message":"billing daily count exceeded"}`, "daily"},
		{"duplicate", 400, `{"message":"duplicate request"}`, "duplicate"},
		{"agent limit with pricing link", 401, `{"message":"{\"agentLimitResetTime\":1999999999000,\"pricingUrl\":\"https://qoder.com/pricing\"}"}`, "agent-limit"},
		{"entitlement", 403, `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`, "entitlement"},
		{"content policy", 400, `{"message":"DataInspectionFailed"}`, "content-policy"},
		{"credential", 401, `{"message":"unauthorized"}`, "unauthorized"},
		{"session dead under 500", 500, `{"code":"12153","message":"gone"}`, "unauthorized"},
		{"client fault", 400, `{"message":"invalid_request_error: bad"}`, "client-fault"},
		{"throttled", 429, `{"message":"slow down"}`, "retryable"},
		{"provider fault", 503, `{"message":"unavailable"}`, "transient"},
		{"plain rejection", 404, `{"message":"not found"}`, "final"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			httpVerdict := refusalVerdict(classifyStatus(tc.status, "", []byte(tc.body)))
			testutil.Equal(t, httpVerdict, tc.want)

			envelope, err := json.Marshal(map[string]any{"body": tc.body, "statusCodeValue": tc.status})
			testutil.NoError(t, err)
			_, streamErr := consumeStreamObserved(strings.NewReader("data:"+string(envelope)+"\n\n"), nil, nil)
			testutil.Equal(t, refusalVerdict(streamErr), tc.want)
		})
	}
}
