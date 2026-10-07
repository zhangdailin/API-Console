package grok

// Shared HTTP plumbing used by the Build (OAuth CLI) plane and by the account
// bookkeeping code. The retired website client that used to live next to these
// helpers was removed together with the Web SSO plane.

import (
	"compress/flate"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

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

// decodeHTTPResponseBody transparently decodes a compressed upstream body and
// leaves the response reading plaintext.
func decodeHTTPResponseBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if idx := strings.Index(encoding, ","); idx >= 0 {
		encoding = strings.TrimSpace(encoding[:idx])
	}
	if encoding == "" || encoding == "identity" {
		return nil
	}

	original := resp.Body
	var (
		reader  io.Reader
		closers []io.Closer
	)
	switch encoding {
	case "gzip":
		gz, err := gzip.NewReader(original)
		if err != nil {
			return err
		}
		reader = gz
		closers = append(closers, gz)
	case "deflate":
		fr := flate.NewReader(original)
		reader = fr
		closers = append(closers, fr)
	case "br":
		reader = brotli.NewReader(original)
	case "zstd":
		zr, err := zstd.NewReader(original)
		if err != nil {
			return err
		}
		reader = zr
		closers = append(closers, closeFunc(func() error { zr.Close(); return nil }))
	default:
		return nil
	}
	closers = append(closers, original)
	resp.Body = responseBodyCloser{Reader: reader, closers: closers}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	return nil
}

type responseBodyCloser struct {
	io.Reader
	closers []io.Closer
}

type closeFunc func() error

func (fn closeFunc) Close() error { return fn() }

func (r responseBodyCloser) Close() error {
	var firstErr error
	for _, closer := range r.closers {
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
