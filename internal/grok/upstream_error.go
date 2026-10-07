package grok

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/util"
	"strings"
	"time"
)

const maxUpstreamBodyBytes = 4096

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
	return parseRetryAfterHeader(e.header.Get("Retry-After"), time.Now())
}

// newCLIUpstreamError builds a typed error with a sanitized header copy, a
// bounded body, and a CLI-prefixed message.
func newCLIUpstreamError(status int, header http.Header, body []byte) error {
	return &grokUpstreamError{
		status: status,
		header: sanitizeUpstreamHeader(header),
		body:   boundedUpstreamBody(body),
		prefix: "grok cli upstream",
	}
}

// sanitizeUpstreamHeader drops credential-bearing and identity headers so the
// typed error can be logged/surfaced safely while preserving classification
// signals such as CF-Mitigated and WWW-Authenticate.
func sanitizeUpstreamHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	out := make(http.Header, len(header))
	for name, values := range header {
		if isSensitiveUpstreamHeader(name) {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

func isSensitiveUpstreamHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "set-cookie", "cookie", "authorization", "proxy-authorization",
		"x-grok-team-id", "x-grok-user-id", "x-xai-token-auth":
		return true
	}
	return false
}

func boundedUpstreamBody(body []byte) string {
	if len(body) > maxUpstreamBodyBytes {
		return string(body[:maxUpstreamBodyBytes])
	}
	return string(body)
}

// readBoundedResponse drains the response body up to maxUpstreamBodyBytes,
// clones the headers (so classification still works after the body is closed),
// and closes the body. It returns the bounded body and the header copy for
// building a typed upstream error. Used on every non-OK response path.
func readBoundedResponse(resp *http.Response) ([]byte, http.Header) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBodyBytes+1))
	if len(raw) > maxUpstreamBodyBytes {
		raw = raw[:maxUpstreamBodyBytes]
	}
	headerCopy := resp.Header.Clone()
	_ = resp.Body.Close()
	return raw, headerCopy
}

// upstreamStatus reads HTTP evidence from typed failures, including OAuth
// failures and wrapped synthetic cooldowns. Local prose carries no status.
func upstreamStatus(err error) int {
	var upstream *grokUpstreamError
	if errors.As(err, &upstream) {
		return upstream.status
	}
	var oauth *cliOAuthError
	if errors.As(err, &oauth) {
		return oauth.status
	}
	return 0
}
