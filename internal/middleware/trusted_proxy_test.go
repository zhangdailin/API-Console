package middleware

import (
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"testing"
)

func TestTrustedProxyMiddlewareRejectsSpoofedForwardingHeaders(t *testing.T) {
	middleware, err := TrustedProxyMiddleware([]string{"10.0.0.0/8"})
	testutil.NoError(t, err)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.Equal(t, ClientIP(r), "203.0.113.10")
		testutil.Equal(t, r.Header.Get("X-Forwarded-For"), "")
		testutil.Equal(t, r.Header.Get("X-Forwarded-Proto"), "")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.10:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Proto", "https")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestTrustedProxyMiddlewareWalksForwardedChain(t *testing.T) {
	middleware, err := TrustedProxyMiddleware([]string{"10.0.0.0/8", "192.0.2.10"})
	testutil.NoError(t, err)
	handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		testutil.Equal(t, ClientIP(r), "198.51.100.9")
		testutil.Equal(t, r.Header.Get("X-Forwarded-Proto"), "https")
		testutil.Equal(t, r.Header.Get("X-Forwarded-For"), "198.51.100.9")
		testutil.Equal(t, r.Header.Get("X-Forwarded-Host"), "api.example.com")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:443"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 192.0.2.10")
	req.Header.Set("X-Forwarded-Proto", "javascript, https")
	req.Header.Set("X-Forwarded-Host", "evil.example, api.example.com")
	handler.ServeHTTP(httptest.NewRecorder(), req)
}

func TestTrustedProxyMiddlewareRejectsInvalidNetwork(t *testing.T) {
	if _, err := TrustedProxyMiddleware([]string{"0.0.0.0/0"}); err != nil {
		// A broad network is syntactically valid and deliberately explicit.
		t.Fatalf("unexpected error: %v", err)
	}
	_, err := TrustedProxyMiddleware([]string{"not-an-ip"})
	testutil.Error(t, err)
}

// A forwarded client address is only honoured when the peer is a trusted proxy,
// while untrusted peers cannot change their attributed source.
func TestTrustedProxyResolvesOnlyTrustedClientAddress(t *testing.T) {
	wrap, err := TrustedProxyMiddleware([]string{"127.0.0.1/32"})
	testutil.NoError(t, err, "TrustedProxyMiddleware: %v")
	var resolved string
	handler := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved = ClientIP(r)
		w.WriteHeader(http.StatusOK)
	}))

	// A trusted peer's forwarded address is the client.
	trusted := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	trusted.RemoteAddr = "127.0.0.1:5555"
	trusted.Header.Set("X-Forwarded-For", "203.0.113.20")
	handler.ServeHTTP(httptest.NewRecorder(), trusted)
	testutil.Equal(t, resolved, "203.0.113.20")

	// The same header from an untrusted peer is cleared, so the peer itself decides.
	untrusted := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	untrusted.RemoteAddr = "203.0.113.9:5555"
	untrusted.Header.Set("X-Forwarded-For", "203.0.113.20")
	handler.ServeHTTP(httptest.NewRecorder(), untrusted)
	testutil.Equal(t, resolved, "203.0.113.9")
}

// Behind Cloudflare the original client is named by CF-Connecting-IP, and only a
// trusted peer's value counts: a direct caller cannot name itself.
func TestTrustedProxyPrefersCloudflareClientHeader(t *testing.T) {
	wrap, err := TrustedProxyMiddleware([]string{"127.0.0.1/32", "173.245.48.0/20"})
	testutil.NoError(t, err, "TrustedProxyMiddleware: %v")
	var resolved string
	handler := wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resolved = ClientIP(r)
		w.WriteHeader(http.StatusOK)
	}))

	// Caddy on loopback forwards Cloudflare's client header, while the forwarded
	// chain only carries edge addresses.
	request := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	request.RemoteAddr = "127.0.0.1:5555"
	request.Header.Set("CF-Connecting-IP", "203.0.113.20")
	request.Header.Set("X-Forwarded-For", "173.245.48.9")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	testutil.Equal(t, resolved, "203.0.113.20")

	// A request from an untrusted peer has the header stripped, so the peer is the
	// client and cannot claim to be someone else.
	direct := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	direct.RemoteAddr = "203.0.113.9:5555"
	direct.Header.Set("CF-Connecting-IP", "203.0.113.20")
	handler.ServeHTTP(httptest.NewRecorder(), direct)
	testutil.Equal(t, resolved, "203.0.113.9")

	// With Cloudflare's ranges trusted, the forwarded chain also resolves to the
	// original client past the edge.
	chain := httptest.NewRequest(http.MethodPost, "http://example.com/v1/chat/completions", nil)
	chain.RemoteAddr = "127.0.0.1:5555"
	chain.Header.Set("X-Forwarded-For", "203.0.113.20, 173.245.48.9")
	handler.ServeHTTP(httptest.NewRecorder(), chain)
	testutil.Equal(t, resolved, "203.0.113.20")
}
