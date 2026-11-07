package web

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

const maxUpstreamBodyBytes = 4096

// grokUpstreamError is a typed Grok upstream failure that preserves the HTTP
// status, a sanitized copy of the response headers, and a bounded body so
// callers can classify Cloudflare/DPoP/account-block without re-parsing
// err.Error() text. Error() keeps the legacy text shape ("grok upstream
// status=N body=...") so existing string matchers keep working.
type grokUpstreamError struct {
	status int
	header http.Header
	body   string
	nodeID string
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
	if e.nodeID != "" {
		b.WriteString(" node=" + e.nodeID)
	}
	if e.body != "" {
		b.WriteString(" body=" + e.body)
	}
	return b.String()
}

// newUpstreamError builds a typed error with a sanitized header copy and a
// bounded body.
func newUpstreamError(status int, header http.Header, body []byte, nodeID string) error {
	return &grokUpstreamError{
		status: status,
		header: sanitizeUpstreamHeader(header),
		body:   boundedUpstreamBody(body),
		nodeID: nodeID,
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
		"dpop", "x-grok-team-id", "x-grok-user-id", "x-xai-token-auth":
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
