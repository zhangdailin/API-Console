package grokbase

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

// Both Grok planes redact typed upstream errors through this one list, so a
// header that is sensitive on either plane (dpop is Web-only today) is dropped
// on both.
func TestSanitizeUpstreamHeaderDropsCredentialAndIdentityHeaders(t *testing.T) {
	in := http.Header{}
	for _, name := range []string{"Set-Cookie", "Cookie", "Authorization", "Proxy-Authorization",
		"DPoP", "X-Grok-Team-Id", "X-Grok-User-Id", "X-Xai-Token-Auth"} {
		in.Set(name, "secret")
	}
	in.Set("Retry-After", "7")
	in.Set("Cf-Mitigated", "challenge")

	out := SanitizeUpstreamHeader(in)
	testutil.Equal(t, len(out), 2)
	testutil.Equal(t, out.Get("Retry-After"), "7")
	testutil.Equal(t, out.Get("Cf-Mitigated"), "challenge")
	testutil.True(t, SanitizeUpstreamHeader(nil) == nil)
}

func TestReadBoundedResponseTruncatesAndCloses(t *testing.T) {
	body := &closeRecorder{Reader: strings.NewReader(strings.Repeat("x", MaxUpstreamBodyBytes+100))}
	resp := &http.Response{Body: body, Header: http.Header{"Retry-After": {"3"}}}
	raw, header := ReadBoundedResponse(resp)
	testutil.Equal(t, len(raw), MaxUpstreamBodyBytes)
	testutil.Equal(t, header.Get("Retry-After"), "3")
	testutil.True(t, body.closed)
	testutil.Equal(t, len(BoundedUpstreamBody(make([]byte, MaxUpstreamBodyBytes+1))), MaxUpstreamBodyBytes)
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }
