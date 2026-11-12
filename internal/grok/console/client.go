// Package console is the Grok Console plane of the grok channel: console.x.ai
// reached with a grok.com SSO cookie. The cookie mints a DPoP-bound access
// token (POST /v1/dpop/token) and every /v1 request carries that token, a
// fresh DPoP proof and the team's API key id (x-apikey).
package console

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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

// ── DPoP ───────────────────────────────────────────────────────────────────

type dpopJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type dpopSession struct {
	accessToken string
	privateKey  *ecdsa.PrivateKey
	publicJWK   dpopJWK
	expiresAt   time.Time
	clockSkew   time.Duration
}

func (c *Client) cachedSession(key string) (dpopSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	session, ok := c.sessions[key]
	if ok && session.expiresAt.After(time.Now().Add(dpopRefreshSkew)) {
		return session, true
	}
	delete(c.sessions, key)
	return dpopSession{}, false
}

func (c *Client) storeSession(key string, session dpopSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sessions) >= maxDPoPSessions {
		now := time.Now().Add(dpopRefreshSkew)
		for k, s := range c.sessions {
			if !s.expiresAt.After(now) {
				delete(c.sessions, k)
			}
		}
		for k := range c.sessions {
			if len(c.sessions) < maxDPoPSessions {
				break
			}
			delete(c.sessions, k)
		}
	}
	c.sessions[key] = session
}

func (c *Client) invalidateSession(key, accessToken string) {
	c.mu.Lock()
	if session, ok := c.sessions[key]; ok && session.accessToken == accessToken {
		delete(c.sessions, key)
	}
	c.mu.Unlock()
}

// session returns a cached DPoP session for the credential, minting one when
// none is valid. Concurrent misses share one mint.
func (c *Client) session(ctx context.Context, ssoToken string) (dpopSession, string, error) {
	key := sessionKey(ssoToken)
	if session, ok := c.cachedSession(key); ok {
		return session, key, nil
	}
	result := c.fetches.DoChan("dpop|"+key, func() (any, error) {
		if session, ok := c.cachedSession(key); ok {
			return session, nil
		}
		session, err := c.mintSession(context.WithoutCancel(ctx), ssoToken)
		if err == nil {
			c.storeSession(key, session)
		}
		return session, err
	})
	select {
	case <-ctx.Done():
		return dpopSession{}, key, ctx.Err()
	case done := <-result:
		if done.Err != nil {
			return dpopSession{}, key, done.Err
		}
		return done.Val.(dpopSession), key, nil
	}
}

func (c *Client) mintSession(ctx context.Context, ssoToken string) (dpopSession, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return dpopSession{}, err
	}
	jwk := publicDPoPJWK(&key.PublicKey)
	body, _ := json.Marshal(map[string]any{"jwk": jwk})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/dpop/token"), bytes.NewReader(body))
	if err != nil {
		return dpopSession{}, err
	}
	c.applyBrowserHeaders(req.Header, ssoToken)
	req.Header.Set("Content-Type", "application/json")
	before := time.Now().UTC()
	resp, err := c.httpClient.Do(req)
	after := time.Now().UTC()
	if err != nil {
		return dpopSession{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return dpopSession{}, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return dpopSession{}, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return dpopSession{}, &StatusError{Status: resp.StatusCode, Message: upstreamErrorMessage(raw)}
	}
	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return dpopSession{}, errors.New("the Console DPoP token response is not JSON")
	}
	if out.AccessToken == "" || !strings.EqualFold(out.TokenType, "DPoP") || out.ExpiresIn <= 0 || time.Duration(out.ExpiresIn)*time.Second > maxDPoPLifetime {
		return dpopSession{}, errors.New("the Console DPoP token response is invalid")
	}
	thumbprint := dpopJWKThumbprint(jwk)
	tokenExpiry, tokenThumbprint, err := parseDPoPAccessToken(out.AccessToken)
	if err != nil || tokenThumbprint != thumbprint {
		return dpopSession{}, errors.New("the Console DPoP token is not bound to this key")
	}
	now := time.Now().UTC()
	expires := now.Add(time.Duration(out.ExpiresIn) * time.Second)
	if tokenExpiry.Before(expires) {
		expires = tokenExpiry
	}
	if !expires.After(now.Add(dpopRefreshSkew)) {
		return dpopSession{}, errors.New("the Console DPoP token is already expiring")
	}
	return dpopSession{accessToken: out.AccessToken, privateKey: key, publicJWK: jwk, expiresAt: expires, clockSkew: dpopClockSkew(resp.Header.Get("Date"), before, after)}, nil
}

func publicDPoPJWK(key *ecdsa.PublicKey) dpopJWK {
	return dpopJWK{
		Kty: "EC", Crv: "P-256",
		X: base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
		Y: base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
	}
}

// dpopJWKThumbprint is the RFC 7638 thumbprint: members in lexical order.
func dpopJWKThumbprint(jwk dpopJWK) string {
	canonical := `{"crv":"` + jwk.Crv + `","kty":"` + jwk.Kty + `","x":"` + jwk.X + `","y":"` + jwk.Y + `"}`
	digest := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func parseDPoPAccessToken(value string) (time.Time, string, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return time.Time{}, "", errors.New("invalid DPoP access token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", errors.New("invalid DPoP access token payload")
	}
	var claims struct {
		ExpiresAt int64 `json:"exp"`
		CNF       struct {
			JKT string `json:"jkt"`
		} `json:"cnf"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.ExpiresAt <= 0 || strings.TrimSpace(claims.CNF.JKT) == "" {
		return time.Time{}, "", errors.New("invalid DPoP access token claims")
	}
	return time.Unix(claims.ExpiresAt, 0).UTC(), claims.CNF.JKT, nil
}

// dpopClockSkew estimates server time minus local time from the mint
// response's Date header; proof iat is shifted by it.
func dpopClockSkew(date string, before, after time.Time) time.Duration {
	server, err := http.ParseTime(strings.TrimSpace(date))
	if err != nil {
		return 0
	}
	if after.Before(before) {
		after = before
	}
	mid := before.Add(after.Sub(before) / 2)
	return server.UTC().Sub(mid).Round(time.Second)
}

// dpopHTU is the proof's target URI: scheme, host and path, no query.
func dpopHTU(u *url.URL) string {
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	scheme := u.Scheme
	switch scheme {
	case "wss":
		scheme = "https"
	case "ws":
		scheme = "http"
	}
	return scheme + "://" + u.Host + path
}

func signDPoPProof(session dpopSession, method string, target *url.URL) (string, error) {
	header, _ := json.Marshal(map[string]any{"alg": "ES256", "typ": "dpop+jwt", "jwk": session.publicJWK})
	tokenDigest := sha256.Sum256([]byte(session.accessToken))
	claims, _ := json.Marshal(map[string]any{
		"jti": util.RandomHex(16), "htm": strings.ToUpper(method), "htu": dpopHTU(target),
		"iat": time.Now().UTC().Add(session.clockSkew).Unix(),
		"ath": base64.RawURLEncoding.EncodeToString(tokenDigest[:]),
	})
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, session.privateKey, digest[:])
	if err != nil {
		return "", err
	}
	signature := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func applyDPoP(header http.Header, session dpopSession, method string, target *url.URL) error {
	proof, err := signDPoPProof(session, method, target)
	if err != nil {
		return err
	}
	header.Set("Authorization", "DPoP "+session.accessToken)
	header.Set("DPoP", proof)
	return nil
}

// ── API key ────────────────────────────────────────────────────────────────

// apiKey returns the team's API key id, sent as x-apikey on every /v1
// inference request (free Console teams are refused without it). It is read
// from the Console home page and created only when the team has none, because
// CreateApiKey has no idempotency guard.
func (c *Client) apiKey(ctx context.Context, ssoToken string, refresh bool) (string, error) {
	key := sessionKey(ssoToken)
	if !refresh {
		c.mu.Lock()
		value := c.apiKeys[key]
		c.mu.Unlock()
		if value != "" {
			return value, nil
		}
	}
	value, err, _ := c.fetches.Do("apikey|"+key, func() (any, error) {
		resolved, err := c.resolveAPIKey(context.WithoutCancel(ctx), ssoToken)
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		c.apiKeys[key] = resolved
		c.mu.Unlock()
		return resolved, nil
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

func (c *Client) forgetAPIKey(ssoToken string) {
	c.mu.Lock()
	delete(c.apiKeys, sessionKey(ssoToken))
	c.mu.Unlock()
}

func (c *Client) resolveAPIKey(ctx context.Context, ssoToken string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	home, err := c.readHome(ctx, ssoToken)
	if err != nil {
		return "", err
	}
	if home.apiKeyID != "" {
		return home.apiKeyID, nil
	}
	if home.teamID == "" {
		return "", fmt.Errorf("%w: no team id", errNoAPIKeyFound)
	}
	if err := c.createAPIKey(ctx, ssoToken, home.teamID); err != nil {
		return "", err
	}
	// The create response carries the secret key, not its id: re-read the
	// page so only the id is ever used.
	confirmed, err := c.readHome(ctx, ssoToken)
	if err != nil {
		return "", err
	}
	if confirmed.apiKeyID == "" {
		return "", fmt.Errorf("%w after creating one", errNoAPIKeyFound)
	}
	return confirmed.apiKeyID, nil
}

type consoleHome struct{ apiKeyID, teamID string }

// readHome loads the Console landing page, which redirects to /team/<id>.
// Only same-origin hops are followed.
func (c *Client) readHome(ctx context.Context, ssoToken string) (consoleHome, error) {
	current, err := url.Parse(c.baseURL() + "/")
	if err != nil {
		return consoleHome{}, err
	}
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	extra := map[string]string{}
	for hop := 0; hop <= homeMaxRedirects; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, current.String(), nil)
		if err != nil {
			return consoleHome{}, err
		}
		c.applyBrowserHeaders(req.Header, ssoToken)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Del("Origin")
		req.Header.Del("Referer")
		for name, value := range extra {
			req.Header.Set("Cookie", req.Header.Get("Cookie")+"; "+name+"="+value)
		}
		resp, err := client.Do(req)
		if err != nil {
			return consoleHome{}, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			for _, cookie := range resp.Cookies() {
				if cookie.Name != "sso" && cookie.Name != "sso-rw" && !strings.ContainsAny(cookie.Name+cookie.Value, ";\r\n") {
					extra[cookie.Name] = cookie.Value
				}
			}
			_ = resp.Body.Close()
			next, err := current.Parse(resp.Header.Get("Location"))
			if err != nil || resp.Header.Get("Location") == "" || !strings.EqualFold(next.Host, current.Host) {
				return consoleHome{}, fmt.Errorf("the Console home page redirected off console.x.ai (status %d)", resp.StatusCode)
			}
			current = next
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxControlBody))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			return consoleHome{}, ErrUnauthorized
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return consoleHome{}, &StatusError{Status: resp.StatusCode, Message: "the Console home page could not be read"}
		}
		if readErr != nil {
			return consoleHome{}, readErr
		}
		home := parseHome(data)
		if match := teamPathPattern.FindStringSubmatch(current.Path); match != nil {
			home.teamID = strings.ToLower(match[1])
		}
		return home, nil
	}
	return consoleHome{}, errors.New("the Console home page redirected too many times")
}

// parseHome extracts the first enabled apiKeyId and the team id from the
// page's embedded (escaped) JSON. UUIDs hold no backslashes, so dropping them
// yields plain JSON fragments to scan.
func parseHome(data []byte) consoleHome {
	text := string(bytes.ReplaceAll(data, []byte(`\`), nil))
	var home consoleHome
	matches := apiKeyIDPattern.FindAllStringSubmatchIndex(text, -1)
	for index, match := range matches {
		end := len(text)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		end = min(end, match[1]+apiKeyScanDistance)
		if disabledPattern.MatchString(text[match[1]:end]) {
			continue
		}
		home.apiKeyID = strings.ToLower(text[match[2]:match[3]])
		break
	}
	if match := teamIDPattern.FindStringSubmatch(text); match != nil {
		home.teamID = strings.ToLower(match[1])
	} else if match := teamPathPattern.FindStringSubmatch(text); match != nil {
		home.teamID = strings.ToLower(match[1])
	}
	return home
}

func (c *Client) createAPIKey(ctx context.Context, ssoToken, teamID string) error {
	target := c.baseURL() + createAPIKeyPath
	resp, err := c.dpopRequest(ctx, ssoToken, http.MethodPost, target, createAPIKeyBody(teamID), requestOptions{
		contentType: "application/connect+proto", referer: c.baseURL() + "/team/" + url.PathEscape(teamID) + "/chat",
	})
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxControlBody))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Status: resp.StatusCode, Message: "Console CreateApiKey was refused"}
	}
	return nil
}

// createAPIKeyBody is the Connect envelope (flag byte, big-endian length)
// around CreateApiKeyRequest{name=1, team_id=2, acls=3...}.
func createAPIKeyBody(teamID string) []byte {
	var payload []byte
	field := func(number int, value string) {
		payload = append(payload, byte(number<<3|2))
		length := len(value)
		for length >= 0x80 {
			payload = append(payload, byte(length)|0x80)
			length >>= 7
		}
		payload = append(payload, byte(length))
		payload = append(payload, value...)
	}
	field(1, apiKeyName)
	field(2, teamID)
	field(3, "api-key:endpoint:*")
	field(3, "api-key:model:*")
	length := len(payload)
	envelope := []byte{0, byte(length >> 24), byte(length >> 16), byte(length >> 8), byte(length)}
	return append(envelope, payload...)
}

func isMissingAPIKeyBody(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "provision an api key") || strings.Contains(text, "not the owner")
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
