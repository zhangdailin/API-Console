package grokbase

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type videoRedirectTransport func(*http.Request) (*http.Response, error)

func (f videoRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVideoRecoveryCookieStaysAtOriginalOrigin(t *testing.T) {
	for _, tt := range []struct {
		redirect string
		allowed  bool
	}{
		{"https://cdn.assets.grok.com/video", true},
		{"https://other.example.test/video", false},
	} {
		t.Run(tt.redirect, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: videoRedirectTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				switch requests {
				case 1:
					if r.Header.Get("Cookie") == "" {
						t.Fatal("origin did not receive its fixture cookie")
					}
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {tt.redirect}}, Body: http.NoBody}, nil
				case 2:
					if !tt.allowed || r.Header.Get("Cookie") != "" {
						t.Fatal("cookie reached a different origin, or an untrusted host was contacted")
					}
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"video/mp4"}}, ContentLength: 4, Body: io.NopCloser(strings.NewReader("file"))}, nil
				default:
					t.Fatal("unexpected video request")
					return nil, nil
				}
			})}
			body, _, err := OpenVideoWithOriginCookie(t.Context(), client, "https://assets.grok.com/video", "session=fixture")
			if tt.allowed {
				if err != nil || requests != 2 {
					t.Fatalf("trusted redirect failed: %v (requests: %d)", err, requests)
				}
				body.Close()
			} else if err == nil || requests != 1 {
				t.Fatalf("untrusted redirect accepted: %v (requests: %d)", err, requests)
			}
		})
	}
}
