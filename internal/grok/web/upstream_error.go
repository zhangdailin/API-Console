package web

import (
	"fmt"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"strings"
)

// grokUpstreamError is a typed Grok upstream failure that preserves the HTTP
// status, a sanitized copy of the response headers, and a bounded body so
// callers can classify Cloudflare/DPoP/account-block without re-parsing
// err.Error() text. Error() keeps the legacy text shape ("grok upstream
// status=N body=...") so existing string matchers keep working.
type grokUpstreamError struct {
	status int
	header http.Header
	body   string
	prefix string // e.g. "grok cli upstream"; defaults to "grok upstream"
}

func (e *grokUpstreamError) Error() string {
	prefix := e.prefix
	if prefix == "" {
		prefix = "grok upstream"
	}
	var b strings.Builder
	b.WriteString(prefix)
	fmt.Fprintf(&b, " status=%d", e.status)
	if e.body != "" {
		b.WriteString(" body=" + e.body)
	}
	return b.String()
}

// newUpstreamError builds a typed error with a sanitized header copy and a
// bounded body.
func newUpstreamError(status int, header http.Header, body []byte) error {
	return &grokUpstreamError{
		status: status,
		header: grokbase.SanitizeUpstreamHeader(header),
		body:   grokbase.BoundedUpstreamBody(body),
	}
}

// upstreamErrorBody extracts the body portion of a legacy plain error's text.
func upstreamErrorBody(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	_, after, ok := strings.Cut(raw, "body=")
	if !ok {
		return ""
	}
	return after
}
