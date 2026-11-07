package grok

// Shared HTTP plumbing for Grok Build and account bookkeeping.

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"orchids-api/internal/config"
)

// newHTTPClient builds the shared browser-fingerprinted HTTP client.
func newHTTPClient(cfg *config.Config, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	proxyKey := "direct"
	if cfg != nil {
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
	}

	return httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey, timeout, 0, proxyFunc)
}

// TokenFingerprint is a short, non-reversible digest of a credential, used in
// logs and refresh bookkeeping so a token itself never reaches a log line.
func TokenFingerprint(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return fmt.Sprintf("%x", sum[:8])
}

// credentialAffinity derives a stable, non-reversible egress affinity key from
// an account identity so one credential keeps hitting the same egress node.
func credentialAffinity(identity string) string {
	normalized := strings.TrimSpace(identity)
	if normalized == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("cred_%x", sum[:16])
}
