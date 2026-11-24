package grokbase

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
)

// PrepareUpstreamResponse decodes the body and applies the idle policy to
// successful responses. The caller owns transport errors and deadline mapping.
func PrepareUpstreamResponse(resp *http.Response, idle time.Duration, wrapIdle func(io.ReadCloser, time.Duration) io.ReadCloser) error {
	if err := httpclient.DecodeContentEncoding(resp); err != nil {
		_ = resp.Body.Close()
		return fmt.Errorf("grok upstream decode failed: %w", err)
	}
	if idle > 0 && resp.StatusCode >= 200 && resp.StatusCode < 300 && resp.Body != nil {
		resp.Body = wrapIdle(resp.Body, idle)
	}
	return nil
}

// NewBrowserClient selects the shared browser transport for a Grok config.
// It does not set a separate response-header timeout.
func NewBrowserClient(cfg *config.Config, timeout time.Duration) *http.Client {
	key := "direct"
	if cfg != nil {
		key = httpclient.GenerateProxyKeyFromConfig(cfg)
	}
	return httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(key, timeout, 0, httpclient.ProxyFuncFromConfig(cfg))
}

// RandomRequestID returns the UUID v4 carried in Grok request headers.
func RandomRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4],
		buf[4:6],
		buf[6:8],
		buf[8:10],
		buf[10:16],
	)
}
