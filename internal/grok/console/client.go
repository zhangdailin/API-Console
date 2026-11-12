// Package console is the Grok Console plane of the grok channel: console.x.ai
// reached with a grok.com SSO cookie. The cookie mints a DPoP-bound access
// token (POST /v1/dpop/token) and every /v1 request carries that token, a
// fresh DPoP proof and the team's API key id (x-apikey).
package console

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/util"
)

const (
	defaultBaseURL        = "https://console.x.ai"
	dpopRefreshSkew       = 20 * time.Second
	maxDPoPLifetime       = time.Hour
	maxDPoPSessions       = 1024
	maxControlBody        = 8 << 20
	apiKeyHeader          = "x-apikey"
	apiKeyName            = "xAI Chat Playground"
	createAPIKeyPath      = "/auth_mgmt.AuthManagement/CreateApiKey"
	homeMaxRedirects      = 3
	apiKeyScanDistance    = 4096
	consoleClusterHeader  = "https://us-east-1.api.x.ai"
	defaultRequestTimeout = 10 * time.Minute
)

var (
	// ErrUnauthorized reports that console.x.ai refused the SSO session.
	ErrUnauthorized = errors.New("console.x.ai did not accept the SSO session")

	apiKeyIDPattern  = regexp.MustCompile(`"apiKeyId"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	disabledPattern  = regexp.MustCompile(`"disabled"\s*:\s*true`)
	teamPathPattern  = regexp.MustCompile(`/team/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)
	teamIDPattern    = regexp.MustCompile(`"teamId"\s*:\s*"([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})"`)
	errNoAPIKeyFound = errors.New("the Console home page offers no usable API key id")
)

// Client talks to console.x.ai. It is safe for concurrent use.
type Client struct {
	cfg        *config.Config
	httpClient *http.Client

	mu       sync.Mutex
	sessions map[string]dpopSession
	apiKeys  map[string]string
	workos   map[string]workosSession
	fetches  singleflight.Group
}

// NewClient builds a Console client over the grok.com transport.
func NewClient(cfg *config.Config) *Client {
	transport := cfg.GrokWebTransport()
	proxyKey := "direct"
	if transport != nil {
		proxyKey = httpclient.GenerateProxyKeyFromConfig(transport)
	}
	return &Client{
		cfg:        cfg,
		httpClient: httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout("console|"+proxyKey, defaultRequestTimeout, 0, httpclient.ProxyFuncFromConfig(transport)),
		sessions:   map[string]dpopSession{},
		apiKeys:    map[string]string{},
		workos:     map[string]workosSession{},
	}
}

func (c *Client) baseURL() string {
	if c != nil && c.cfg != nil && strings.TrimSpace(c.cfg.GrokConsoleBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.cfg.GrokConsoleBaseURL), "/")
	}
	return defaultBaseURL
}

func (c *Client) endpoint(path string) string {
	return c.baseURL() + "/v1/" + strings.TrimLeft(strings.TrimSpace(path), "/")
}

func (c *Client) userAgent() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokWebUserAgentOrDefault()
	}
	return config.DefaultGrokWebUserAgent
}

// cookieHeader is the browser cookie console.x.ai reads the session from,
// with the configured Cloudflare clearance.
func (c *Client) cookieHeader(ssoToken string) string {
	parts := []string{"sso=" + ssoToken, "sso-rw=" + ssoToken}
	if c != nil && c.cfg != nil {
		if value := strings.TrimSpace(c.cfg.GrokConfigCFClearance); value != "" {
			parts = append(parts, "cf_clearance="+value)
		}
		if value := strings.TrimSpace(c.cfg.GrokConfigCFBM); value != "" {
			parts = append(parts, "__cf_bm="+value)
		}
	}
	return strings.Join(parts, "; ")
}

func (c *Client) applyBrowserHeaders(header http.Header, ssoToken string) {
	header.Set("Accept", "*/*")
	header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Pragma", "no-cache")
	header.Set("Cookie", c.cookieHeader(ssoToken))
	header.Set("Origin", c.baseURL())
	header.Set("Referer", c.baseURL()+"/")
	header.Set("Sec-Fetch-Dest", "empty")
	header.Set("Sec-Fetch-Mode", "cors")
	header.Set("Sec-Fetch-Site", "same-origin")
	header.Set("User-Agent", c.userAgent())
}

// sessionKey identifies a credential in the in-memory caches without
// keeping the cookie itself as a map key.
func sessionKey(ssoToken string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(ssoToken)))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// ── Requests ───────────────────────────────────────────────────────────────

// StatusError is a non-2xx Console answer. Message is upstream prose bounded
// to a short summary; it never carries the request's credentials.
type StatusError struct {
	Status     int
	Message    string
	RetryAfter string
}

func (e *StatusError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("console.x.ai returned status %d", e.Status)
	}
	return fmt.Sprintf("console.x.ai returned status %d: %s", e.Status, e.Message)
}

type requestOptions struct {
	contentType string
	accept      string
	referer     string
	withAPIKey  bool
}

// Do sends one /v1 request for the credential. A 2xx response is returned
// open for the caller to stream; any other status is returned as *StatusError
// with the body consumed.
func (c *Client) Do(ctx context.Context, ssoToken, method, path string, body []byte, contentType, accept string) (*http.Response, error) {
	resp, err := c.dpopRequest(ctx, ssoToken, method, c.endpoint(path), body, requestOptions{contentType: contentType, accept: accept, withAPIKey: true})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrUnauthorized
	}
	return nil, &StatusError{Status: resp.StatusCode, Message: upstreamErrorMessage(raw), RetryAfter: resp.Header.Get("Retry-After")}
}

func (c *Client) dpopRequest(ctx context.Context, ssoToken, method, target string, body []byte, options requestOptions) (*http.Response, error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	apiKey := ""
	apiKeyRepaired := false
	if options.withAPIKey {
		// Without a key the upstream still answers with a classified error,
		// which says more than a local failure would.
		if apiKey, err = c.apiKey(ctx, ssoToken, false); err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			apiKeyRepaired = true
		}
	}
	dpopRetried := false
	for attempt := 0; attempt < 3; attempt++ {
		session, key, err := c.session(ctx, ssoToken)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		c.applyBrowserHeaders(req.Header, ssoToken)
		if len(body) > 0 {
			req.Header.Set("Content-Type", util.FirstNonEmpty(options.contentType, "application/json"))
		}
		if options.accept != "" {
			req.Header.Set("Accept", options.accept)
		}
		if options.referer != "" {
			req.Header.Set("Referer", options.referer)
		}
		if strings.HasSuffix(parsed.Path, "/responses") {
			req.Header.Set("x-cluster", consoleClusterHeader)
		}
		if apiKey != "" {
			req.Header.Set(apiKeyHeader, apiKey)
		}
		if err := applyDPoP(req.Header, session, method, parsed); err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && !dpopRetried {
			// An expired or revoked DPoP session: mint a new one once.
			dpopRetried = true
			_ = resp.Body.Close()
			c.invalidateSession(key, session.accessToken)
			continue
		}
		if options.withAPIKey && !apiKeyRepaired && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden) {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if isMissingAPIKeyBody(raw) {
				apiKeyRepaired = true
				c.forgetAPIKey(ssoToken)
				if repaired, repairErr := c.apiKey(ctx, ssoToken, true); repairErr == nil && repaired != "" {
					apiKey = repaired
					continue
				}
			}
			resp.Body = io.NopCloser(bytes.NewReader(raw))
			return resp, nil
		}
		return resp, nil
	}
	return nil, errors.New("console.x.ai kept refusing the DPoP session")
}

// WebSocketHeaders returns the headers that authenticate a WebSocket upgrade
// to target (a ws:// or wss:// /v1 URL). DPoP signs the https form of the URI.
func (c *Client) WebSocketHeaders(ctx context.Context, ssoToken, target string) (http.Header, func(), error) {
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, nil, err
	}
	session, key, err := c.session(ctx, ssoToken)
	if err != nil {
		return nil, nil, err
	}
	header := http.Header{}
	c.applyBrowserHeaders(header, ssoToken)
	header.Set("Sec-Fetch-Mode", "websocket")
	if apiKey, keyErr := c.apiKey(ctx, ssoToken, false); keyErr == nil && apiKey != "" {
		header.Set(apiKeyHeader, apiKey)
	}
	if err := applyDPoP(header, session, http.MethodGet, parsed); err != nil {
		return nil, nil, err
	}
	return header, func() { c.invalidateSession(key, session.accessToken) }, nil
}

// WebSocketURL is the ws(s):// form of a /v1 path on the Console origin.
func (c *Client) WebSocketURL(path string, query url.Values) (string, error) {
	parsed, err := url.Parse(c.endpoint(path))
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// HTTPClient is the transport Console traffic uses, for downloads of
// finished media.
func (c *Client) HTTPClient() *http.Client { return c.httpClient }

// FetchModels reads the credential's model catalog (GET /v1/models).
func (c *Client) FetchModels(ctx context.Context, ssoToken string) ([]string, error) {
	resp, err := c.Do(ctx, ssoToken, http.MethodGet, "/models", nil, "", "application/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
	if err != nil {
		return nil, err
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			ID string `json:"id"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errors.New("the Console model list is not JSON")
	}
	seen := map[string]bool{}
	var out []string
	for _, entry := range append(payload.Data, payload.Models...) {
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id != "" && !seen[id] && !strings.ContainsAny(id, " /") {
			seen[id] = true
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the Console model list is empty")
	}
	return out, nil
}

// upstreamErrorMessage extracts a short message from an error body.
func upstreamErrorMessage(raw []byte) string {
	var payload map[string]any
	message := ""
	if json.Unmarshal(raw, &payload) == nil {
		switch value := payload["error"].(type) {
		case map[string]any:
			message, _ = value["message"].(string)
		case string:
			message = value
		}
		if message == "" {
			message, _ = payload["message"].(string)
		}
	}
	message = strings.TrimSpace(message)
	if len(message) > 200 {
		message = strings.ToValidUTF8(message[:200], "")
	}
	return message
}
