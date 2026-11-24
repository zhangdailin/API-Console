package grokbase

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/httpserver"
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

// UpstreamErrorText preserves the text used by Grok's classifiers and audit.
func UpstreamErrorText(prefix string, status int, body string) string {
	text := prefix + " status=" + strconv.Itoa(status)
	if body != "" {
		text += " body=" + body
	}
	return text
}

// IsUpstreamTransportFailure recognizes shared network and legacy upstream
// failures. Each plane still decides which of its own typed errors count.
func IsUpstreamTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"upstream", "connection reset", "connection refused", "broken pipe", "no such host", "tls handshake"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// WritePublicUpstreamError exposes the shared, redacted Chat error projection.
// A credential error belongs to the operator's account pool, not the caller.
func WritePublicUpstreamError(w http.ResponseWriter, err error, retryAfter int, report func(error, int, string)) {
	text := err.Error()
	category := apperrors.ClassifyUpstreamError(text).Category
	status := apperrors.StatusForCategory(category)
	switch category {
	case "auth", "auth_blocked", "configuration":
		status = http.StatusServiceUnavailable
	}
	if report != nil {
		report(err, status, category)
	}
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	httpserver.WriteErrorCode(w, status, category, apperrors.PublicMessage(text))
}
