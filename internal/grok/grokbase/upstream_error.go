package grokbase

import (
	"io"
	"net/http"
	"strings"
)

// MaxUpstreamBodyBytes bounds the upstream error body kept on a typed failure
// and in attempt diagnostics. Both Grok planes share one ceiling.
const MaxUpstreamBodyBytes = 4096

// SanitizeUpstreamHeader drops credential-bearing and identity headers so a
// typed upstream error can be logged or surfaced safely while keeping
// classification signals such as CF-Mitigated and WWW-Authenticate.
func SanitizeUpstreamHeader(header http.Header) http.Header {
	if header == nil {
		return nil
	}
	out := make(http.Header, len(header))
	for name, values := range header {
		if IsSensitiveUpstreamHeader(name) {
			continue
		}
		out[name] = append([]string(nil), values...)
	}
	return out
}

// IsSensitiveUpstreamHeader reports whether a header carries a credential or
// an account identity. The list is shared by both Grok planes on purpose: a
// redaction rule must not depend on two copies being kept in sync by hand.
func IsSensitiveUpstreamHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "set-cookie", "cookie", "authorization", "proxy-authorization",
		"dpop", "x-grok-team-id", "x-grok-user-id", "x-xai-token-auth":
		return true
	}
	return false
}

// BoundedUpstreamBody truncates an upstream body to MaxUpstreamBodyBytes.
func BoundedUpstreamBody(body []byte) string {
	if len(body) > MaxUpstreamBodyBytes {
		return string(body[:MaxUpstreamBodyBytes])
	}
	return string(body)
}

// ReadBoundedResponse drains the response body up to MaxUpstreamBodyBytes,
// clones the headers (so classification still works after the body is
// closed) and closes the body. Every non-OK response path uses it to build a
// typed upstream error.
func ReadBoundedResponse(resp *http.Response) ([]byte, http.Header) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxUpstreamBodyBytes+1))
	if len(raw) > MaxUpstreamBodyBytes {
		raw = raw[:MaxUpstreamBodyBytes]
	}
	headerCopy := resp.Header.Clone()
	_ = resp.Body.Close()
	return raw, headerCopy
}
