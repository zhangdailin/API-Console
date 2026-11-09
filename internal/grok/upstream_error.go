package grok

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/util"
	"strings"
	"time"
)

// Only this structured rejection proves that the request was refused before
// generation. A bare 429, transport error or successful tool turn cannot prove
// that replaying a hosted tool request is safe.
func isFreeUsageExhaustedError(err error) bool {
	var upstream *grokUpstreamError
	if !errors.As(err, &upstream) || upstream == nil || upstream.status != http.StatusTooManyRequests {
		return false
	}
	return hasFreeUsageExhaustionCode([]byte(upstream.body))
}

func hasFreeUsageExhaustionCode(body []byte) bool {
	var rejection struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &rejection) != nil {
		return false
	}
	if rejection.Code != "" {
		return rejection.Code == "subscription:free-usage-exhausted"
	}
	var nested struct {
		Code string `json:"code"`
	}
	return json.Unmarshal(rejection.Error, &nested) == nil && nested.Code == "subscription:free-usage-exhausted"
}

// grokUpstreamError is a typed Grok upstream failure that preserves the HTTP
// status, a sanitized copy of the response headers, and a bounded body so
// callers can classify upstream/account-block without re-parsing
// err.Error() text. Error() renders status and body for internal diagnostics and
// the shared account-policy classifier.
type grokUpstreamError struct {
	status int
	header http.Header
	body   string
	prefix string // e.g. "grok cli upstream"; defaults to "grok upstream"
}

func (e *grokUpstreamError) Error() string {
	prefix := e.prefix
	prefix = util.FirstNonEmptyUntrimmed(prefix, "grok upstream")
	var b strings.Builder
	b.WriteString(prefix)
	fmt.Fprintf(&b, " status=%d", e.status)
	if e.body != "" {
		b.WriteString(" body=" + e.body)
	}
	return b.String()
}

func (e *grokUpstreamError) RetryAfter() time.Duration {
	if e == nil || e.header == nil {
		return 0
	}
	return ratelimit.ParseRetryAfter(e.header.Get("Retry-After"), time.Now())
}

// newCLIUpstreamError builds a typed error with a sanitized header copy, a
// bounded body, and a CLI-prefixed message.
func newCLIUpstreamError(status int, header http.Header, body []byte) error {
	return &grokUpstreamError{
		status: status,
		header: grokbase.SanitizeUpstreamHeader(header),
		body:   grokbase.BoundedUpstreamBody(body),
		prefix: "grok cli upstream",
	}
}

// upstreamStatus reads HTTP evidence from typed failures, including OAuth
// failures and wrapped synthetic cooldowns. Local prose carries no status.
func upstreamStatus(err error) int {
	if upstream, ok := errors.AsType[*grokUpstreamError](err); ok {
		return upstream.status
	}
	if oauth, ok := errors.AsType[*cliOAuthError](err); ok {
		return oauth.status
	}
	return 0
}
