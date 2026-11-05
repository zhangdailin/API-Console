// Statsig signature client.
//
// Derived from chenyme/grok2api, commit 906b9493. MIT license:
// ../../licenses/third-party-MIT.txt. Source:
// backend/internal/infra/provider/web/statsig.go.
//
// Grok's web plane expects an `x-statsig-id` that is derived from the account's
// own page metadata by a signer. This gateway previously omitted the header
// rather than fabricate a token, which is honest but leaves the request without
// a value the upstream expects; the reference implementation instead asks an
// external signer for one and keeps it for an hour, re-signing after a
// challenge. Both parts are ported here: the page read uses the same browser
// transport and cookies as the request, and the signing endpoint is validated
// before it is called.
package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"orchids-api/internal/config"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/sync/singleflight"
)

// DefaultStatsigSignerURL is grok2api's own signing endpoint, and the one this
// gateway uses when the deployment has not chosen another. It is exported so the
// admin plane can show which endpoint a configuration is actually using.
const DefaultStatsigSignerURL = "https://grok.wodf.de/sign"

// statsigDefaultEndpointOverride lets a test exercise the unset case against a
// local signer instead of the reference endpoint. Production never sets it.
var statsigDefaultEndpointOverride string

const (
	statsigCacheTTL        = time.Hour
	statsigCacheMaxEntries = 4096
	statsigMetaBodyLimit   = 4 << 20
	statsigResponseLimit   = 4 << 10
	statsigMetaTimeout     = 15 * time.Second
	statsigSignerTimeout   = 12 * time.Second
	statsigMetaName        = "grok-site-verification"
)

var errStatsigMetaMissing = fmt.Errorf("grok index is missing the %s meta tag", statsigMetaName)

type statsigCacheEntry struct {
	value     string
	expiresAt time.Time
}

// statsigSigner caches one signature per (base URL, signer, method, path).
type statsigSigner struct {
	client    *http.Client
	now       func() time.Time
	mu        sync.Mutex
	entries   map[string]statsigCacheEntry
	refreshes singleflight.Group
}

func newStatsigSigner() *statsigSigner {
	return &statsigSigner{
		client: &http.Client{
			Timeout:       statsigSignerTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now:     time.Now,
		entries: make(map[string]statsigCacheEntry),
	}
}

// signature returns the id to send, and where it came from. do issues the page
// read with the account's own transport, so the meta content belongs to the
// account that is about to use the id.
func (s *statsigSigner) signature(ctx context.Context, do func(*http.Request) (*http.Response, error), baseURL, signerURL, cookie string, method, target string) (string, string, error) {
	if s == nil {
		return "", "", fmt.Errorf("statsig signer unavailable")
	}
	key, path, err := statsigSignatureKey(baseURL, signerURL, method, target)
	if err != nil {
		return "", "", err
	}
	if value, ok := s.cached(key, s.now().UTC()); ok {
		return value, "cache", nil
	}
	value, err, _ := s.refreshes.Do(key, func() (interface{}, error) {
		now := s.now().UTC()
		if cached, ok := s.cached(key, now); ok {
			return statsigSignResult{value: cached, source: "cache"}, nil
		}
		fresh, refreshErr := s.freshSignature(ctx, do, baseURL, signerURL, cookie, method, path)
		if refreshErr != nil {
			if stale, ok := s.stale(key); ok {
				return statsigSignResult{value: stale, source: "stale"}, nil
			}
			return statsigSignResult{}, refreshErr
		}
		s.store(key, fresh, now.Add(statsigCacheTTL), now)
		return statsigSignResult{value: fresh, source: "refresh"}, nil
	})
	if err != nil {
		return "", "", err
	}
	result, _ := value.(statsigSignResult)
	return result.value, result.source, nil
}

type statsigSignResult struct {
	value  string
	source string
}

// freshSignature reads the page once, signs, and re-reads the page before one
// retry: a signer can reject a stale metaContent, and re-fetching is cheaper than
// failing the request.
func (s *statsigSigner) freshSignature(ctx context.Context, do func(*http.Request) (*http.Response, error), baseURL, signerURL, cookie, method, path string) (string, error) {
	meta, err := s.fetchMetaContent(ctx, do, baseURL, cookie)
	if err != nil {
		return "", err
	}
	signature, err := s.requestSignature(ctx, signerURL, method, path, meta)
	if err == nil {
		return signature, nil
	}
	meta, refreshErr := s.fetchMetaContent(ctx, do, baseURL, cookie)
	if refreshErr != nil {
		return "", fmt.Errorf("refresh statsig metaContent: %w", refreshErr)
	}
	signature, retryErr := s.requestSignature(ctx, signerURL, method, path, meta)
	if retryErr != nil {
		return "", fmt.Errorf("statsig signing failed: %w", retryErr)
	}
	return signature, nil
}

func (s *statsigSigner) cached(key string, now time.Time) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || entry.value == "" || !now.Before(entry.expiresAt) {
		return "", false
	}
	return entry.value, true
}

// stale returns the last value even after it expires, because a rejected id is
// still more likely to work than no id at all.
func (s *statsigSigner) stale(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	return entry.value, ok && validStatsigID(entry.value)
}

func (s *statsigSigner) store(key, value string, expiresAt, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing, entry := range s.entries {
		if !now.Before(entry.expiresAt) {
			delete(s.entries, existing)
		}
	}
	if len(s.entries) >= statsigCacheMaxEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for existing, entry := range s.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldestExpiry) {
				oldestKey, oldestExpiry = existing, entry.expiresAt
			}
		}
		delete(s.entries, oldestKey)
	}
	s.entries[key] = statsigCacheEntry{value: value, expiresAt: expiresAt}
}

// statsigSignatureKey identifies one signature. The path is the escaped path only,
// so query strings cannot multiply cache entries.
func statsigSignatureKey(baseURL, signerURL, method, target string) (key, path string, err error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", "", fmt.Errorf("parse statsig target: %w", err)
	}
	path = parsed.EscapedPath()
	if path == "" {
		path = "/"
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	return strings.TrimRight(baseURL, "/") + "\x00" + strings.TrimSpace(signerURL) + "\x00" + method + "\x00" + path, path, nil
}

func (s *statsigSigner) requestSignature(ctx context.Context, endpoint, method, path, metaContent string) (string, error) {
	if err := config.ValidateStatsigSignerURL(endpoint); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"method": strings.ToUpper(strings.TrimSpace(method)),
		"path":   path,
		"environment": map[string]string{
			"metaContent": metaContent,
		},
	})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, statsigResponseLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > statsigResponseLimit {
		return "", fmt.Errorf("signer response exceeds the safety limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("signer returned %d", response.StatusCode)
	}
	var value struct {
		StatsigID string `json:"x-statsig-id"`
	}
	if json.Unmarshal(body, &value) != nil || !validStatsigID(value.StatsigID) {
		return "", fmt.Errorf("signer response is not a valid statsig id")
	}
	return value.StatsigID, nil
}

// fetchMetaContent reads the account's index page and extracts the verification
// meta. grok.com currently answers /index with a branded 404 that still carries
// the meta; only that exact status may fall back to the canonical root page.
func (s *statsigSigner) fetchMetaContent(ctx context.Context, do func(*http.Request) (*http.Response, error), baseURL, cookie string) (string, error) {
	index, err := s.fetchMetaResponse(ctx, do, baseURL, cookie, "/index")
	if err != nil {
		return "", err
	}
	if index.statusCode >= 200 && index.statusCode < 300 {
		return extractStatsigMetaContent(index.body)
	}
	if index.statusCode != http.StatusNotFound {
		return "", fmt.Errorf("grok index returned %d", index.statusCode)
	}
	if content, extractErr := extractStatsigMetaContent(index.body); extractErr == nil {
		return content, nil
	}
	root, err := s.fetchMetaResponse(ctx, do, baseURL, cookie, "/")
	if err != nil {
		return "", err
	}
	if root.statusCode < 200 || root.statusCode >= 300 {
		return "", fmt.Errorf("grok index page returned %d", root.statusCode)
	}
	return extractStatsigMetaContent(root.body)
}

type statsigMetaResponse struct {
	statusCode int
	body       []byte
}

func (s *statsigSigner) fetchMetaResponse(ctx context.Context, do func(*http.Request) (*http.Response, error), baseURL, cookie, path string) (statsigMetaResponse, error) {
	if do == nil {
		return statsigMetaResponse{}, fmt.Errorf("statsig page read has no transport")
	}
	requestCtx, cancel := context.WithTimeout(ctx, statsigMetaTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return statsigMetaResponse{}, err
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("Sec-Fetch-Dest", "document")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	if strings.TrimSpace(cookie) != "" {
		request.Header.Set("Cookie", cookie)
	}
	response, err := do(request)
	if err != nil {
		return statsigMetaResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, statsigMetaBodyLimit+1))
	if err != nil {
		return statsigMetaResponse{}, err
	}
	if len(body) > statsigMetaBodyLimit {
		return statsigMetaResponse{}, fmt.Errorf("grok index page exceeds the safety limit")
	}
	return statsigMetaResponse{statusCode: response.StatusCode, body: body}, nil
}

// extractStatsigMetaContent finds the verification meta in the page. A page
// without it is a protocol failure, not an empty value: the caller must be able
// to tell a changed upstream from a successful read.
func extractStatsigMetaContent(body []byte) (string, error) {
	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if tokenizer.Err() == io.EOF {
				return "", errStatsigMetaMissing
			}
			return "", tokenizer.Err()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttrs := tokenizer.TagName()
			if !strings.EqualFold(string(name), "meta") || !hasAttrs {
				continue
			}
			metaName := ""
			content := ""
			for {
				key, value, more := tokenizer.TagAttr()
				switch strings.ToLower(string(key)) {
				case "name":
					metaName = strings.ToLower(strings.TrimSpace(string(value)))
				case "content":
					content = strings.TrimSpace(string(value))
				}
				if !more {
					break
				}
			}
			if metaName == statsigMetaName && content != "" {
				return content, nil
			}
		}
	}
}

// validStatsigID is grok2api's check: the id is base64 that decodes to exactly 70
// bytes. A shorter value is a fabricated or truncated token.
func validStatsigID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 70
}
