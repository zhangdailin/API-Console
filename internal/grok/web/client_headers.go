package web

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// statsigID is the manually configured value, used when no signer is set.
func (c *Client) statsigID() string {
	if c != nil && c.cfg != nil && strings.TrimSpace(c.cfg.GrokStatsigID) != "" {
		// grok2api's own validity rule: base64 that decodes to 70 bytes. The
		// browser-shaped prefix check was stricter than the reference and rejected
		// values the upstream accepts.
		if configured := strings.TrimSpace(c.cfg.GrokStatsigID); validStatsigID(configured) {
			return configured
		}
	}
	// With no value and no signer the header is omitted: a locally fabricated
	// TypeError token is more fingerprintable than an absent optional header and
	// cannot be refreshed.
	return ""
}

// statsigSignerOnce is the process-wide signer: signatures are keyed by page and
// path, so sharing the cache across accounts avoids re-reading grok.com per
// credential.
var (
	statsigSignerOnce sync.Once
	statsigSignerInst *statsigSigner
)

func (c *Client) statsigSigner() *statsigSigner {
	statsigSignerOnce.Do(func() { statsigSignerInst = newStatsigSigner() })
	return statsigSignerInst
}

// statsigSignerURL is the signing endpoint this deployment uses.
//
// Unset means grok2api's own default, so an upgraded deployment signs exactly
// like the reference without being configured. An explicit empty string is the
// opt-out, and any other value is that endpoint.
func (c *Client) statsigSignerURL() string {
	if c == nil || c.cfg == nil {
		return statsigDefaultEndpoint()
	}
	if c.cfg.GrokStatsigSignerURL == nil {
		return statsigDefaultEndpoint()
	}
	return strings.TrimSpace(*c.cfg.GrokStatsigSignerURL)
}

// statsigDefaultEndpoint is grok2api's endpoint, or a test's local stand-in.
func statsigDefaultEndpoint() string {
	if override := strings.TrimSpace(statsigDefaultEndpointOverride); override != "" {
		return override
	}
	return DefaultStatsigSignerURL
}

// statsigIDForRequest returns the header value for one request: the signed one
// when a signer is configured, otherwise the manual value. A signing failure is
// never fatal — the request proceeds without the header, which is exactly what
// this gateway did before the signer existed.
func (c *Client) statsigIDForRequest(ctx context.Context, token, method, reqURL string) string {
	manual := c.statsigID()
	signerURL := c.statsigSignerURL()
	if signerURL == "" {
		return manual
	}
	signer := c.statsigSigner()
	if signer == nil {
		return manual
	}
	cookie := ""
	if cfClearance, cfBM := c.cloudflareCookies(); true {
		cookie = buildGrokCookie(token, cfClearance, cfBM)
	}
	value, source, err := signer.signature(ctx, c.statsigMetaDoer(token), c.baseURL(), signerURL, cookie, method, reqURL)
	if err != nil || value == "" {
		if err != nil {
			slog.Debug("statsig signing failed; sending the request without the header", "error", err, "path", reqURL)
		}
		return manual
	}
	slog.Debug("statsig id resolved", "source", source, "path", reqURL)
	return value
}

// statsigMetaDoer reads the account's own page through the same transport the
// account's requests use, so the metaContent belongs to this account.
func (c *Client) statsigMetaDoer(token string) func(*http.Request) (*http.Response, error) {
	client := c.clientForAsset(false)
	if client == nil {
		client = http.DefaultClient
	}
	return func(request *http.Request) (*http.Response, error) {
		return client.Do(request)
	}
}

func (c *Client) cloudflareCookies() (string, string) {
	if c == nil || c.cfg == nil {
		return "", ""
	}
	cfClearance := strings.TrimSpace(c.cfg.GrokConfigCFClearance)
	cfBM := strings.TrimSpace(c.cfg.GrokConfigCFBM)
	return cfClearance, cfBM
}

// baseHeaders 是预分配的请求头模板，包含所有固定的请求头
// 这避免了在每次请求时重新分配这些固定头，提升性能
var baseHeaders = http.Header{
	"Accept":             []string{"*/*"},
	"Accept-Language":    []string{"zh-CN,zh;q=0.9,en;q=0.8"},
	"Baggage":            []string{"sentry-environment=production,sentry-release=d6add6fb0460641fd482d767a335ef72b9b6abb8,sentry-public_key=b311e0f2690c81f25e2c4cf6d4f7ce1c"},
	"Cache-Control":      []string{"no-cache"},
	"Content-Type":       []string{"application/json"},
	"Origin":             []string{"https://grok.com"},
	"Pragma":             []string{"no-cache"},
	"Referer":            []string{"https://grok.com/"},
	"Priority":           []string{"u=1, i"},
	"Sec-Ch-Ua":          []string{defaultAppChatSecCHUA},
	"Sec-Ch-Ua-Mobile":   []string{"?0"},
	"Sec-Ch-Ua-Platform": []string{`"macOS"`},
	"Sec-Fetch-Dest":     []string{"empty"},
	"Sec-Fetch-Mode":     []string{"cors"},
	"Sec-Fetch-Site":     []string{"same-origin"},
}

var appChatHeaders = http.Header{
	"Accept":             []string{"*/*"},
	"Accept-Encoding":    []string{"gzip, deflate, br, zstd"},
	"Accept-Language":    []string{"zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7"},
	"Cache-Control":      []string{"no-cache"},
	"Content-Type":       []string{"application/json"},
	"Origin":             []string{"https://grok.com"},
	"Pragma":             []string{"no-cache"},
	"Priority":           []string{"u=1, i"},
	"Referer":            []string{"https://grok.com/"},
	"Sec-Ch-Ua":          []string{defaultAppChatSecCHUA},
	"Sec-Ch-Ua-Mobile":   []string{"?0"},
	"Sec-Ch-Ua-Platform": []string{`"macOS"`},
	"Sec-Fetch-Dest":     []string{"empty"},
	"Sec-Fetch-Mode":     []string{"cors"},
	"Sec-Fetch-Site":     []string{"same-origin"},
}

func cloneHeaderShallow(src http.Header, extraCapacity int) http.Header {
	if extraCapacity < 0 {
		extraCapacity = 0
	}
	if len(src) == 0 {
		return make(http.Header, extraCapacity)
	}
	dst := make(http.Header, len(src)+extraCapacity)
	maps.Copy(dst, src)
	return dst
}

type grokCookieItem struct {
	name  string
	value string
}

func grokCookieItems(raw string) []grokCookieItem {
	parts := strings.Split(strings.TrimSpace(raw), ";")
	out := make([]grokCookieItem, 0, len(parts))
	for _, part := range parts {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(name)), " "))
		value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
		if name == "" {
			continue
		}
		out = append(out, grokCookieItem{name: name, value: value})
	}
	return out
}

func isSetCookieAttribute(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "path", "domain", "expires", "max-age", "secure", "httponly", "samesite", "partitioned", "priority":
		return true
	default:
		return false
	}
}

func setGrokCookieItem(items *[]grokCookieItem, name, value string) {
	name = strings.ToLower(strings.TrimSpace(name))
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return
	}
	for i := range *items {
		if (*items)[i].name == name {
			(*items)[i].value = value
			return
		}
	}
	*items = append(*items, grokCookieItem{name: name, value: value})
}

func joinGrokCookieItems(items []grokCookieItem) string {
	if len(items) == 0 {
		return ""
	}
	var b strings.Builder
	for _, item := range items {
		if strings.TrimSpace(item.name) == "" || strings.TrimSpace(item.value) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(item.name)
		b.WriteString("=")
		b.WriteString(item.value)
	}
	return b.String()
}

func buildGrokCookie(token, cfClearance, cfBM string) string {
	raw := strings.TrimSpace(token)
	parsed := grokCookieItems(raw)
	sso := ""
	for _, item := range parsed {
		if item.name == "sso" {
			sso = item.value
			break
		}
	}
	if sso == "" && raw != "" && !strings.Contains(raw, "=") {
		sso = strings.Join(strings.Fields(raw), " ")
	}

	items := make([]grokCookieItem, 0, len(parsed)+4)
	if sso != "" {
		setGrokCookieItem(&items, "sso", sso)
		setGrokCookieItem(&items, "sso-rw", sso)
		for _, item := range parsed {
			if item.name == "sso" || item.name == "sso-rw" || isSetCookieAttribute(item.name) {
				continue
			}
			setGrokCookieItem(&items, item.name, item.value)
		}
	} else {
		for _, item := range parsed {
			if isSetCookieAttribute(item.name) {
				continue
			}
			setGrokCookieItem(&items, item.name, item.value)
		}
	}
	setGrokCookieItem(&items, "cf_clearance", cfClearance)
	setGrokCookieItem(&items, "__cf_bm", cfBM)
	return joinGrokCookieItems(items)
}

func appChatDeviceEnvInfo() map[string]any {
	return map[string]any{
		"darkModeEnabled":  true,
		"devicePixelRatio": 1.75,
		"screenWidth":      2560,
		"screenHeight":     1440,
		"viewportWidth":    899,
		"viewportHeight":   726,
	}
}

func applyChromiumClientHints(header http.Header, userAgent string) {
	header.Del("Sec-Ch-Ua")
	header.Del("Sec-Ch-Ua-Mobile")
	header.Del("Sec-Ch-Ua-Platform")
	ua := strings.TrimSpace(userAgent)
	marker := "Chrome/"
	_, after, ok := strings.Cut(ua, marker)
	if !ok {
		return
	}
	version := after
	if end := strings.IndexAny(version, ". "); end >= 0 {
		version = version[:end]
	}
	if version == "" {
		return
	}
	brand := "Google Chrome"
	if strings.Contains(ua, "Chromium/") && !strings.Contains(ua, "Chrome/") {
		brand = "Chromium"
	}
	platform := "Unknown"
	switch {
	case strings.Contains(ua, "Windows"):
		platform = "Windows"
	case strings.Contains(ua, "Macintosh") || strings.Contains(ua, "Mac OS X"):
		platform = "macOS"
	case strings.Contains(ua, "Linux"):
		platform = "Linux"
	}
	header.Set("Sec-Ch-Ua", fmt.Sprintf(`"%s";v="%s", "Chromium";v="%s", "Not(A:Brand";v="24"`, brand, version, version))
	header.Set("Sec-Ch-Ua-Mobile", "?0")
	header.Set("Sec-Ch-Ua-Platform", strconv.Quote(platform))
}

// headersFor is headers with the statsig id resolved for one request. The plain
// headers method keeps the manual-only behaviour for callers without a context.
func (c *Client) headersFor(ctx context.Context, token, method, reqURL string) http.Header {
	h := c.headers(token)
	if statsig := c.statsigIDForRequest(ctx, token, method, reqURL); statsig != "" {
		h.Set("x-statsig-id", statsig)
	} else {
		h.Del("x-statsig-id")
	}
	return h
}

func (c *Client) headers(token string) http.Header {
	// 从预分配的模板浅克隆请求头；固定值切片复用，动态字段再覆盖。
	h := cloneHeaderShallow(baseHeaders, 4)

	// 添加动态请求头
	h.Set("User-Agent", c.userAgent())
	applyChromiumClientHints(h, c.userAgent())
	if statsig := c.statsigID(); statsig != "" {
		h.Set("x-statsig-id", statsig)
	}
	h.Set("x-xai-request-id", randomUUID())

	// 构建 Cookie
	cfClearance, cfBM := c.cloudflareCookies()
	if cookie := buildGrokCookie(token, cfClearance, cfBM); cookie != "" {
		h.Set("Cookie", cookie)
	}

	return h
}

func (c *Client) appChatHeaders(token string) http.Header {
	return c.appChatHeadersWithReferer(token, "")
}

// appChatHeadersFor resolves the statsig id for one app-chat request.
func (c *Client) appChatHeadersFor(ctx context.Context, token, method, reqURL string) http.Header {
	h := c.appChatHeaders(token)
	if statsig := c.statsigIDForRequest(ctx, token, method, reqURL); statsig != "" {
		h.Set("x-statsig-id", statsig)
	} else {
		h.Del("x-statsig-id")
	}
	return h
}

func (c *Client) appChatHeadersWithReferer(token, referer string) http.Header {
	h := cloneHeaderShallow(appChatHeaders, 4)
	h.Set("User-Agent", c.userAgent())
	applyChromiumClientHints(h, c.userAgent())
	if referer = strings.TrimSpace(referer); referer != "" {
		h.Set("Referer", referer)
	}
	if statsig := c.statsigID(); statsig != "" {
		h.Set("x-statsig-id", statsig)
	}
	h.Set("x-xai-request-id", randomUUID())

	cfClearance, cfBM := c.cloudflareCookies()
	if cookie := buildGrokCookie(token, cfClearance, cfBM); cookie != "" {
		h.Set("Cookie", cookie)
	}
	return h
}

// anonymousAssetHosts are media/CDN hosts that serve signed, short-lived URLs.
// They must never receive the session cookie: the credential is bound to the
// API origin, so attaching it to a CDN request copies it to a third-party log
// surface. grok2api downloads these anonymously (Accept/User-Agent only).
// assets.grok.com is deliberately *not* listed — that host is served from the
// same session as the API and is verified to carry auth in practice.
var anonymousAssetHosts = []string{
	"imagine-public.x.ai",
	"imgen.x.ai",
	"vidgen.x.ai",
}

func isAnonymousAssetHost(host string) bool {
	for _, base := range anonymousAssetHosts {
		if host == base || strings.HasSuffix(host, "."+base) {
			return true
		}
	}
	return false
}

func (c *Client) shouldSendAuthForAssetURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return false
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return false
	}
	if isAnonymousAssetHost(host) {
		return false
	}
	if host == "grok.com" || strings.HasSuffix(host, ".grok.com") || host == "x.ai" || strings.HasSuffix(host, ".x.ai") {
		return true
	}
	for _, base := range []string{c.baseURL(), defaultAssetsBaseURL} {
		bu, err := url.Parse(strings.TrimSpace(base))
		if err != nil || bu == nil {
			continue
		}
		if strings.EqualFold(host, strings.TrimSpace(bu.Hostname())) {
			return true
		}
	}
	return false
}

func (c *Client) assetDownloadHeaders(token, link string) http.Header {
	var headers http.Header
	if c.shouldSendAuthForAssetURL(link) {
		headers = c.headers(token)
		headers.Set("Content-Type", "")
		headers.Set("Accept-Encoding", "identity")
		headers.Set("Referer", "https://grok.com/")
	} else {
		headers = http.Header{
			"Accept":          []string{"image/avif,image/webp,image/apng,image/svg+xml,image/*,video/*,*/*;q=0.8"},
			"Accept-Language": []string{"zh-CN,zh;q=0.9,en;q=0.8"},
			"User-Agent":      []string{c.userAgent()},
			"Accept-Encoding": []string{"identity"},
		}
	}
	return headers
}
