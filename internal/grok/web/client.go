package web

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"encoding/json"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/web/egress"
	"orchids-api/internal/httpclient"
	"orchids-api/internal/util"
)

const (
	defaultBaseURL          = "https://grok.com"
	defaultAssetsBaseURL    = "https://assets.grok.com"
	defaultAssetsListPath   = "/rest/assets"
	defaultAssetsDeletePath = "/rest/assets-metadata"
	defaultChatPath         = "/rest/app-chat/conversations/new"
	defaultConversationPath = "/rest/app-chat/conversations"
	defaultUploadFilePath   = "/rest/app-chat/upload-file"
	defaultCanvasCreatePath = "/rest/media/canvas/create"
	defaultMediaConvoPath   = "/rest/media/conversation/create"
	defaultLivekitPath      = "/rest/livekit/tokens"
	defaultRateLimitsPath   = "/rest/rate-limits"
	defaultAcceptTOSURL     = "https://accounts.x.ai/auth_mgmt.AuthManagement/SetTosAcceptedVersion"
	defaultSetBirthPath     = "/rest/auth/set-birth-date"
	defaultNSFWMgmtPath     = "/auth_mgmt.AuthManagement/UpdateUserFeatureControls"
	defaultUA               = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	defaultAppChatSecCHUA   = `"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"`
)

type Client struct {
	cfg         *config.Config
	httpClient  *http.Client
	assetClient *http.Client
	egress      *egress.Manager
	// appChatTransport is deliberately kept behind a small boundary.  The
	// historical REST request shape and the newer MGW websocket request shape
	// are not wire-compatible, so callers must never silently reinterpret an
	// app-chat payload as an MGW event stream.
	appChatTransport appChatTransport
}

type NSFWEnableResult struct {
	Success     bool   `json:"success"`
	HTTPStatus  int    `json:"http_status"`
	GRPCStatus  int    `json:"grpc_status,omitempty"`
	GRPCMessage string `json:"grpc_message,omitempty"`
	Error       string `json:"error,omitempty"`
}

func New(cfg *config.Config) *Client {
	cfg = cfg.GrokWebTransport()
	timeout := cfg.GrokWebRequestTimeout()
	baseClient := newHTTPClient(cfg, timeout, httpclient.ProxyFuncFromConfig(cfg))
	client := &Client{
		cfg:         cfg,
		httpClient:  baseClient,
		assetClient: baseClient,
		egress:      egress.NewManager(cfg),
	}
	// MGW is available through the explicit request API in
	// mgw_websocket_transport.go.  Legacy callers only provide a REST payload,
	// therefore the adapter records that MGW is not applicable and safely falls
	// back to the established REST transport.
	client.appChatTransport = newAppChatFallbackTransport(client)
	return client
}

func (c *Client) baseURL() string {
	if c.cfg != nil {
		return c.cfg.GrokWebBaseURLOrDefault()
	}
	return defaultBaseURL
}

func (c *Client) userAgent() string {
	if c.cfg != nil {
		return c.cfg.GrokWebUserAgentOrDefault()
	}
	return defaultUA
}

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
	for k, v := range src {
		dst[k] = v
	}
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

func appChatDeviceEnvInfo() map[string]interface{} {
	return map[string]interface{}{
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
	idx := strings.Index(ua, marker)
	if idx < 0 {
		return
	}
	version := ua[idx+len(marker):]
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

func credentialAffinity(token string) string {
	normalized := NormalizeSSOToken(token)
	if normalized == "" {
		normalized = strings.TrimSpace(token)
	}
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("sso_%x", sum[:16])
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

func (c *Client) chatPayload(spec ModelSpec, text string, noMemory bool, imageCount int) map[string]interface{} {
	cnt := imageCount
	if cnt <= 0 {
		cnt = 2
	}
	temporary := true
	if c != nil && c.cfg != nil {
		temporary = c.cfg.GrokChatTemporary()
		noMemory = c.cfg.GrokChatDisableMemory(noMemory)
	}
	includeModelFields := spec.IsImage || spec.IsVideo
	responseMetadata := map[string]interface{}{}
	if includeModelFields {
		responseMetadata["modelConfigOverride"] = map[string]interface{}{
			"modelMap": map[string]interface{}{},
		}
		responseMetadata["requestModelDetails"] = map[string]interface{}{
			"modelId": spec.UpstreamModel,
		}
	}
	payload := map[string]interface{}{
		"collectionIds":               []string{},
		"disabledConnectorIds":        []string{},
		"temporary":                   temporary,
		"message":                     text,
		"fileAttachments":             []string{},
		"imageAttachments":            []string{},
		"disableSearch":               false,
		"enableImageGeneration":       true,
		"returnImageBytes":            false,
		"enableImageStreaming":        true,
		"imageGenerationCount":        cnt,
		"forceConcise":                false,
		"forceSideBySide":             false,
		"isAsyncChat":                 false,
		"linkQuery":                   false,
		"isReasoning":                 false,
		"disableSelfHarmShortCircuit": false,
		"disableTextFollowUps":        false,
		"returnRawGrokInXaiRequest":   false,
		"enableSideBySide":            true,
		"sendFinalMetadata":           true,
		"responseMetadata":            responseMetadata,
		"disableMemory":               noMemory,
		"deviceEnvInfo":               appChatDeviceEnvInfo(),
	}
	if includeModelFields {
		payload["modelName"] = spec.UpstreamModel
	}
	if imageCount > 0 {
		payload["toolOverrides"] = map[string]interface{}{"imageGen": true}
	}
	if includeModelFields && strings.TrimSpace(spec.ModelMode) != "" {
		payload["modelMode"] = spec.ModelMode
	}
	if modeID := appChatModeID(spec); modeID != "" {
		payload["modeId"] = modeID
	}
	if includeModelFields {
		if tier := appChatModelTier(spec); tier != "" {
			payload["modelTier"] = tier
		}
	}
	if includeModelFields && spec.PreferBest {
		payload["preferBest"] = true
	}
	if c != nil && c.cfg != nil {
		if customPersonality := c.cfg.GrokChatCustomInstruction(); customPersonality != "" {
			payload["customPersonality"] = customPersonality
		}
	}
	return payload
}

func (c *Client) appChatImagePayload(spec ModelSpec, prompt, _ string, _ int) map[string]interface{} {
	message := strings.TrimSpace(prompt)
	if message != "" && !strings.HasPrefix(strings.ToLower(message), "drawing:") {
		message = "Drawing: " + message
	}
	payload := c.chatPayload(spec, message, false, 2)
	payload["modeId"] = "fast"
	payload["imageGenerationCount"] = 2
	return payload
}

func appChatModeID(spec ModelSpec) string {
	return resolveModeID(spec, strings.TrimSpace(spec.ID))
}

// resolveModeID maps ModelSpec mode fields to the upstream mode/model name.
// fallback is used when ModeID, ModelMode, and UpstreamModel are all empty
// (app-chat falls back to the public model ID; rate-limit probes fall back to "fast").
func resolveModeID(spec ModelSpec, fallback string) string {
	if modeID := strings.TrimSpace(spec.ModeID); modeID != "" {
		return modeID
	}
	mode := strings.TrimSpace(spec.ModelMode)
	switch mode {
	case "MODEL_MODE_FAST":
		return "fast"
	case "MODEL_MODE_AUTO":
		return "auto"
	case "MODEL_MODE_EXPERT":
		return "expert"
	case "MODEL_MODE_HEAVY":
		return "heavy"
	case "MODEL_MODE_GROK_4_3":
		return "grok-420-computer-use-sa"
	}
	if mode != "" {
		return mode
	}
	if upstream := strings.TrimSpace(spec.UpstreamModel); upstream != "" {
		return upstream
	}
	return fallback
}

func appChatModelTier(spec ModelSpec) string {
	switch spec.Tier {
	case grokTierBasic:
		return "basic"
	case grokTierLite:
		return "lite"
	case grokTierSuper:
		return "super"
	case grokTierHeavy:
		return "heavy"
	default:
		return ""
	}
}

func (c *Client) clientForAsset(asset bool) *http.Client {
	if asset && c.assetClient != nil {
		return c.assetClient
	}
	return c.httpClient
}

func (c *Client) clientForAppChat() *http.Client {
	if c != nil && c.httpClient != nil {
		return c.httpClient
	}
	return http.DefaultClient
}

type responseBodyCloser struct {
	io.Reader
	closers []io.Closer
}

type closeFunc func() error

func (fn closeFunc) Close() error {
	return fn()
}

func (r responseBodyCloser) Close() error {
	var firstErr error
	for _, closer := range r.closers {
		if closer == nil {
			continue
		}
		if err := closer.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

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
		closers = append(closers, closeFunc(func() error {
			zr.Close()
			return nil
		}))
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

func (c *Client) doRequest(ctx context.Context, reqURL string, method string, body []byte, headers http.Header, asset bool) (*http.Response, error) {
	return c.doRequestWithHTTPClient(ctx, c.clientForAsset(asset), reqURL, method, body, headers, http.StatusOK, true)
}

func (c *Client) doAppChatRequest(ctx context.Context, reqURL string, body []byte, headers http.Header) (*http.Response, error) {
	return c.doRequestWithHTTPClient(ctx, c.clientForAppChat(), reqURL, http.MethodPost, body, headers, http.StatusOK, false)
}

func appChatRateLimitRequest(body []byte, headers http.Header) (token, model string) {
	if headers != nil {
		token = NormalizeSSOToken(headers.Get("Cookie"))
	}
	if len(body) == 0 {
		return token, ""
	}
	var payload map[string]interface{}
	if json.Unmarshal(body, &payload) == nil {
		model = firstNonEmpty(parseLooseStringAny(payload["modelName"]), parseLooseStringAny(payload["model"]))
	}
	return token, model
}

func (c *Client) doRequestWithHTTPClient(ctx context.Context, httpClient *http.Client, reqURL string, method string, body []byte, headers http.Header, okStatus int, retry429 bool) (*http.Response, error) {
	if okStatus == 0 {
		okStatus = http.StatusOK
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	maxRetries := 0
	if c.cfg != nil && c.cfg.MaxRetries > 0 {
		maxRetries = c.cfg.MaxRetries
	}
	baseDelay := 1 * time.Second
	if c.cfg != nil && c.cfg.RetryDelay > 0 {
		baseDelay = time.Duration(c.cfg.RetryDelay) * time.Millisecond
	}
	retry429Delay := time.Duration(0)

	var lastStatus int
	var lastBody string
	lastDelay := baseDelay
	challengeRetried := false

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		reqHeaders := cloneHeaderShallow(headers, 1)
		reqHeaders.Set("x-xai-request-id", randomUUID())
		req.Header = reqHeaders

		// Egress: route through the proxy pool with a bound UA + clearance. When
		// egress is explicitly enabled the manager is fail-closed: an acquire
		// failure is returned, never silently downgraded to the direct client.
		var resp *http.Response
		var lease *egress.Lease
		leaseNodeID := ""
		releaseLease := func() {}
		if c.egress != nil && c.egress.Enabled() {
			affinity := requestCredentialAffinity(req.Header, reqURL)
			lease, err = c.egress.Acquire(ctx, c.egressScopeForURL(reqURL), affinity)
			if err != nil {
				recordEgressAcquireError()
				return nil, fmt.Errorf("grok egress unavailable: %w", err)
			}
			leaseNodeID = lease.NodeID
			req.Header.Set("User-Agent", lease.UserAgent)
			applyChromiumClientHints(req.Header, lease.UserAgent)
			if lease.CFCookies != "" {
				mergeCFCookies(req.Header, lease.CFCookies)
			}
			releaseLease = lease.Release
		}

		do := httpClient.Do
		if lease != nil {
			do = lease.Do
		}
		diagnosticAttempt := debug.BeginUpstream(ctx, method, reqURL, req.Header, body)
		resp, err = doUpstreamHTTP(req, do, c.cfg.GrokWebStreamIdleTimeout(), upstreamIdleBytes)
		diagnosticAttempt.Response(resp, err)
		if err != nil {
			if c.egress != nil && c.egress.Enabled() && leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeTransportError)
			}
			releaseLease()
			if attempt >= maxRetries {
				return nil, err
			}
			delay := backoffDelay(baseDelay, retry429Delay, lastDelay, attempt, 0, 0)
			lastDelay = delay
			if !util.SleepWithContext(ctx, delay) {
				return nil, ctx.Err()
			}
			continue
		}
		resp.Body = diagnosticAttempt.CaptureBody(resp.Body)
		if resp.StatusCode == okStatus {
			if c.egress != nil && c.egress.Enabled() && leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeSuccess)
			}
			if lease != nil {
				resp.Body = &leaseResponseBody{ReadCloser: resp.Body, release: releaseLease}
			}
			return resp, nil
		}

		lastStatus = resp.StatusCode
		raw, headerCopy := readBoundedResponse(resp)
		lastBody = string(raw)

		if lastStatus == http.StatusTooManyRequests {
			// Web/app-chat must use the same scoped registry as Console and Build.
			// noteTeamRateLimit only recorded a raw TeamID key, while request
			// admission checks provider+account/team identities; that mismatch made
			// every later Web request miss the cooldown and hit upstream again.
			token, model := appChatRateLimitRequest(body, headers)
			if meta := noteScopedRateLimit(ctx, ProviderWeb, token, model, lastStatus, resp.Header, raw); meta != nil {
				if desc := meta.Describe(); desc != "" {
					lastBody = lastBody + " " + desc
				}
			}
			if c.egress != nil && c.egress.Enabled() && leaseNodeID != "" {
				c.egress.FeedbackOutcome(leaseNodeID, egress.OutcomeRateLimited)
			}
			releaseLease()
			if retry429 && attempt < maxRetries {
				slog.Debug("doRequest retry triggered", "status", lastStatus, "attempt", attempt, "body", lastBody)
				retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
				delay := backoffDelay(baseDelay, retry429Delay, lastDelay, attempt, lastStatus, retryAfter)
				lastDelay = delay
				if !util.SleepWithContext(ctx, delay) {
					return nil, ctx.Err()
				}
				continue
			}
			return nil, newUpstreamError(lastStatus, headerCopy, []byte(lastBody), leaseNodeID)
		}

		// Response-aware classification: Cloudflare/DPoP challenges are egress
		// problems, never account problems.
		kind := ClassifyUpstreamResponse(lastStatus, resp.Header, raw)
		if kind == UpstreamErrorCloudflareChallenge {
			recordUpstreamChallenge("cloudflare")
			if c.egress != nil && c.egress.Enabled() && !challengeRetried {
				// Invalidate this exact clearance and re-acquire once. The retry
				// does not consume the attempt budget (decrement before continue).
				challengeRetried = true
				if lease != nil {
					lease.InvalidateClearance()
				}
				releaseLease()
				attempt--
				continue
			}
		} else if kind == UpstreamErrorDPoPChallenge {
			recordUpstreamChallenge("dpop")
		} else if kind == UpstreamErrorGenericForbidden {
			recordGenericForbidden()
			if lease != nil {
				lease.InvalidateClearance()
			}
		}

		if c.egress != nil && c.egress.Enabled() && leaseNodeID != "" {
			c.egress.FeedbackOutcome(leaseNodeID, egressOutcomeForKind(kind))
		}
		releaseLease()
		return nil, newUpstreamError(lastStatus, headerCopy, raw, leaseNodeID)
	}

	// Loop always returns; this trailing return satisfies the compiler.
	return nil, newUpstreamError(lastStatus, nil, []byte(lastBody), "")
}

// egressScopeForURL maps an upstream URL to an egress node scope.
func (c *Client) egressScopeForURL(reqURL string) string {
	host := strings.ToLower(strings.TrimSpace(reqURL))
	switch {
	case strings.Contains(host, "cli-chat-proxy.grok.com"):
		return "cli"
	case strings.Contains(host, "console.x.ai"):
		return "console"
	default:
		return "app_chat"
	}
}

// requestCredentialAffinity derives account affinity from the SSO cookie
// already attached to a request. It never exposes the credential in manager
// state or cache keys. Non-authenticated requests retain a URL-specific key.
func requestCredentialAffinity(header http.Header, reqURL string) string {
	for _, item := range grokCookieItems(header.Get("Cookie")) {
		if item.name == "sso" || item.name == "sso-rw" {
			return credentialAffinity(item.value)
		}
	}
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(reqURL))))
	return fmt.Sprintf("url_%x", sum[:12])
}

// mergeCFCookies injects Cloudflare clearance cookies into an existing Cookie
// header without clobbering the SSO cookies already set.
func mergeCFCookies(header http.Header, cfCookies string) {
	if strings.TrimSpace(cfCookies) == "" {
		return
	}
	existing := strings.TrimSpace(header.Get("Cookie"))
	switch {
	case existing == "":
		header.Set("Cookie", cfCookies)
	default:
		header.Set("Cookie", existing+"; "+cfCookies)
	}
}

func (c *Client) doChat(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	if c == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	transport := c.appChatTransport
	if transport == nil {
		transport = restAppChatTransport{}
	}
	return transport.Chat(ctx, c, token, payload)
}

// doRESTChat preserves the proven REST app-chat transport.  It is intentionally
// separate from doChat so the transport selector can fall back without
// recursively calling itself.
func (c *Client) doRESTChat(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	if err := waitScopedRateLimit(ctx, ProviderWeb, token, parseLooseStringAny(payload["modelName"]), c.cfg.GrokWebRequestsPerSecond()); err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultChatPath
	headers := c.appChatHeadersFor(ctx, token, http.MethodPost, reqURL)
	if media, _ := payload["mediaGenInput"].(map[string]interface{}); media["imageToImage"] != nil {
		headers.Set("Referer", c.baseURL()+"/imagine")
	}
	return c.doAppChatRequest(ctx, reqURL, body, headers)
}

func (c *Client) doAppChatCreateAndRespond(ctx context.Context, token string, payload map[string]interface{}) (*http.Response, error) {
	canvasID, err := c.createAppChatCanvas(ctx, token)
	if err != nil {
		return nil, err
	}
	referer := c.appChatImagineReferer(canvasID)
	conversationID, err := c.createAppChatConversation(ctx, token, referer)
	if err != nil {
		return nil, err
	}
	if err := c.linkAppChatConversationToCanvas(ctx, token, conversationID, canvasID); err != nil {
		return nil, err
	}
	return c.doAppChatAddResponse(ctx, token, conversationID, canvasID, payload)
}

func (c *Client) appChatImagineReferer(canvasID string) string {
	id := strings.TrimSpace(canvasID)
	if id == "" {
		return c.baseURL() + "/imagine/agent"
	}
	return c.baseURL() + "/imagine/agent/" + url.PathEscape(id)
}

func (c *Client) createAppChatCanvas(ctx context.Context, token string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"name": "",
	})
	if err != nil {
		return "", err
	}
	reqURL := c.baseURL() + defaultCanvasCreatePath
	headers := c.appChatHeadersWithReferer(token, c.appChatImagineReferer(""))
	if statsig := c.statsigIDForRequest(ctx, token, http.MethodPost, reqURL); statsig != "" {
		headers.Set("x-statsig-id", statsig)
	}
	resp, err := c.doAppChatRequest(ctx, reqURL, body, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("grok app-chat create canvas decode failed: %w", err)
	}
	canvasID := extractAppChatCanvasID(payload)
	if canvasID == "" {
		return "", fmt.Errorf("grok app-chat create canvas returned no canvas id")
	}
	return canvasID, nil
}

func (c *Client) createAppChatConversation(ctx context.Context, token, referer string) (string, error) {
	body, err := json.Marshal(map[string]interface{}{
		"systemPromptName": "",
		"temporary":        false,
	})
	if err != nil {
		return "", err
	}
	reqURL := c.baseURL() + defaultConversationPath
	headers := c.appChatHeadersWithReferer(token, referer)
	if statsig := c.statsigIDForRequest(ctx, token, http.MethodPost, reqURL); statsig != "" {
		headers.Set("x-statsig-id", statsig)
	}
	resp, err := c.doAppChatRequest(ctx, reqURL, body, headers)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var payload map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("grok app-chat create conversation decode failed: %w", err)
	}
	conversationID := extractAppChatConversationID(payload)
	if conversationID == "" {
		return "", fmt.Errorf("grok app-chat create conversation returned no conversation id")
	}
	return conversationID, nil
}

func (c *Client) linkAppChatConversationToCanvas(ctx context.Context, token, conversationID, canvasID string) error {
	conversationID = strings.TrimSpace(conversationID)
	canvasID = strings.TrimSpace(canvasID)
	if conversationID == "" {
		return fmt.Errorf("empty app-chat conversation id")
	}
	if canvasID == "" {
		return fmt.Errorf("empty app-chat canvas id")
	}
	body, err := json.Marshal(map[string]interface{}{
		"conversationId": conversationID,
		"canvasId":       canvasID,
	})
	if err != nil {
		return err
	}
	reqURL := c.baseURL() + defaultMediaConvoPath
	resp, err := c.doAppChatRequest(ctx, reqURL, body, c.appChatHeadersWithReferer(token, c.appChatImagineReferer(canvasID)))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

func (c *Client) doAppChatAddResponse(ctx context.Context, token, conversationID, canvasID string, payload map[string]interface{}) (*http.Response, error) {
	id := strings.TrimSpace(conversationID)
	if id == "" {
		return nil, fmt.Errorf("empty app-chat conversation id")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	reqURL := c.baseURL() + defaultConversationPath + "/" + url.PathEscape(id) + "/responses"
	return c.doAppChatRequest(ctx, reqURL, body, c.appChatHeadersWithReferer(token, c.appChatImagineReferer(canvasID)))
}

func extractAppChatCanvasID(v interface{}) string {
	return extractIDFromValue(v, []string{"canvasId", "canvas_id", "documentId", "document_id", "id"}, []string{"document", "canvas", "result", "data"})
}

func extractAppChatConversationID(v interface{}) string {
	return extractIDFromValue(v, []string{"conversationId", "conversation_id"}, []string{"conversation", "result", "data"})
}

// extractIDFromValue walks nested maps and slices, returning the first
// non-empty string found under any of the idKeys (leaf identifiers), then any
// of the containerKeys whose values may themselves hold the identifier.
func extractIDFromValue(v interface{}, idKeys []string, containerKeys []string) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(x)
	case map[string]interface{}:
		for _, key := range idKeys {
			if id := extractIDFromValue(x[key], idKeys, containerKeys); id != "" {
				return id
			}
		}
		for _, key := range containerKeys {
			if id := extractIDFromValue(x[key], idKeys, containerKeys); id != "" {
				return id
			}
		}
	case []interface{}:
		for _, item := range x {
			if id := extractIDFromValue(item, idKeys, containerKeys); id != "" {
				return id
			}
		}
	}
	return ""
}

func (c *Client) VerifyToken(ctx context.Context, token, modelID string) (*RateLimitInfo, error) {
	return c.GetUsage(ctx, token, modelID)
}

// GetWebQuota reads the Web SSO quota products using the upstream's stable
// mode names. Web rate-limits are keyed by "auto" and "fast", not by the
// public model ID (for example, grok-4.5). A partial result is returned when
// one mode is unavailable; callers can still display the successful window.
func (c *Client) GetWebQuota(ctx context.Context, token string) (map[string]*RateLimitInfo, error) {
	token = strings.TrimSpace(token)
	if NormalizeSSOToken(token) == "" {
		return nil, fmt.Errorf("empty token")
	}
	windows := make(map[string]*RateLimitInfo, 2)
	var errs []string
	for _, mode := range []string{"auto", "fast"} {
		info, err := c.getUsageByMode(ctx, token, mode)
		if err != nil {
			if IsAuthenticationFailure(err) {
				return nil, err
			}
			errs = append(errs, mode+": "+err.Error())
			continue
		}
		if info != nil {
			windows[mode] = info
		}
	}
	if len(windows) > 0 {
		return windows, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("grok web quota unavailable")
	}
	return nil, fmt.Errorf("grok web quota unavailable: %s", strings.Join(errs, "; "))
}

func (c *Client) getUsageByMode(ctx context.Context, token, mode string) (*RateLimitInfo, error) {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return nil, fmt.Errorf("empty quota mode")
	}
	return c.getUsageBySpec(ctx, token, ModelSpec{ModeID: mode})
}

func (c *Client) GetUsage(ctx context.Context, token, modelID string) (*RateLimitInfo, error) {
	token = strings.TrimSpace(token)
	if NormalizeSSOToken(token) == "" {
		return nil, fmt.Errorf("empty token")
	}

	model := strings.TrimSpace(modelID)
	spec, ok := ResolveModel(model)
	if !ok {
		// Fall back to a known default model when the model ID is not recognized
		// (e.g. AgentMode="grok-3" stored on older accounts).
		spec, ok = ResolveModel("grok-4.5")
		if !ok {
			return nil, fmt.Errorf("model not found")
		}
	}
	return c.getUsageBySpec(ctx, token, spec)
}

func (c *Client) getUsageBySpec(ctx context.Context, token string, spec ModelSpec) (*RateLimitInfo, error) {
	payload := map[string]interface{}{
		"requestKind": "DEFAULT",
		"modelName":   rateLimitModelName(spec),
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultRateLimitsPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, c.headersFor(ctx, token, http.MethodPost, reqURL), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var payloadResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&payloadResp); err != nil {
		return nil, err
	}
	if info := parseRateLimitPayload(payloadResp); info != nil {
		return info, nil
	}
	return parseRateLimitInfo(resp.Header), nil
}

func rateLimitModelName(spec ModelSpec) string {
	return resolveModeID(spec, "fast")
}

func (c *Client) uploadFile(ctx context.Context, token, fileName, fileMimeType, contentBase64 string) (string, string, error) {
	payload := map[string]string{
		"fileName":     strings.TrimSpace(fileName),
		"fileMimeType": strings.TrimSpace(fileMimeType),
		"content":      strings.TrimSpace(contentBase64),
	}
	if payload["fileName"] == "" {
		payload["fileName"] = "file.bin"
	}
	if payload["fileMimeType"] == "" {
		payload["fileMimeType"] = "application/octet-stream"
	}
	if payload["content"] == "" {
		return "", "", fmt.Errorf("empty upload content")
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}

	reqURL := c.baseURL() + defaultUploadFilePath
	headers := c.headersFor(ctx, token, http.MethodPost, reqURL)
	headers.Set("Referer", "https://grok.com/files")
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, false)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var out struct {
		FileMetadataID string `json:"fileMetadataId"`
		FileURI        string `json:"fileUri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(out.FileMetadataID) == "" && strings.TrimSpace(out.FileURI) == "" {
		return "", "", fmt.Errorf("grok upload returned empty identifiers")
	}
	return strings.TrimSpace(out.FileMetadataID), strings.TrimSpace(out.FileURI), nil
}

func (c *Client) downloadAsset(ctx context.Context, token, rawURL string) ([]byte, string, error) {
	link := strings.TrimSpace(rawURL)
	if link == "" {
		return nil, "", fmt.Errorf("empty asset url")
	}
	if !strings.HasPrefix(strings.ToLower(link), "http://") && !strings.HasPrefix(strings.ToLower(link), "https://") {
		if !strings.HasPrefix(link, "/") {
			link = "/" + link
		}
		link = defaultAssetsBaseURL + link
	}

	var resp *http.Response
	var err error
	if c.shouldSendAuthForAssetURL(link) {
		resp, err = c.doRequest(ctx, link, http.MethodGet, nil, c.assetDownloadHeaders(token, link), true)
	} else {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if reqErr != nil {
			return nil, "", reqErr
		}
		req.Header = c.assetDownloadHeaders(token, link)
		resp, err = c.clientForAsset(true).Do(req)
		if err == nil && resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return nil, "", fmt.Errorf("asset download status=%d body=%s", resp.StatusCode, string(raw))
		}
	}
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	mime := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	return data, mime, nil
}

func (c *Client) getVoiceToken(ctx context.Context, token, voice, personality string, speed float64, instruction string) (map[string]interface{}, error) {
	if strings.TrimSpace(voice) == "" {
		voice = "ara"
	}
	if strings.TrimSpace(personality) == "" {
		personality = "assistant"
	}
	if speed <= 0 {
		speed = 1.0
	}

	session := map[string]interface{}{
		"voice":          strings.TrimSpace(voice),
		"personality":    strings.TrimSpace(personality),
		"playback_speed": speed,
		"enable_vision":  false,
		"turn_detection": map[string]interface{}{"type": "server_vad"},
	}
	_ = instruction

	sessionPayload, err := json.Marshal(session)
	if err != nil {
		return nil, err
	}

	payload := map[string]interface{}{
		"sessionPayload":       string(sessionPayload),
		"requestAgentDispatch": false,
		"livekitUrl":           "wss://livekit.grok.com",
		"params": map[string]interface{}{
			"enable_markdown_transcript": "true",
		},
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	reqURL := c.baseURL() + defaultLivekitPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, c.headersFor(ctx, token, http.MethodPost, reqURL), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out := map[string]interface{}{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) listAssets(ctx context.Context, token string) ([]string, error) {
	token = strings.TrimSpace(token)
	if NormalizeSSOToken(token) == "" {
		return nil, fmt.Errorf("empty token")
	}

	params := url.Values{}
	params.Set("pageSize", "50")
	params.Set("orderBy", "ORDER_BY_LAST_USE_TIME")
	params.Set("source", "SOURCE_ANY")
	params.Set("isLatest", "true")

	assetIDs := make([]string, 0, 64)
	seenPageTokens := map[string]struct{}{}
	nextPageToken := ""
	for page := 0; page < 500; page++ {
		if page > 0 {
			if strings.TrimSpace(nextPageToken) == "" {
				break
			}
			if _, exists := seenPageTokens[nextPageToken]; exists {
				// Upstream returned a repeated page token; stop to avoid endless loop.
				break
			}
			seenPageTokens[nextPageToken] = struct{}{}
			params.Set("pageToken", nextPageToken)
		} else {
			params.Del("pageToken")
		}

		reqURL := c.baseURL() + defaultAssetsListPath + "?" + params.Encode()
		headers := c.headers(token)
		headers.Set("Origin", "https://grok.com")
		headers.Set("Referer", "https://grok.com/files")
		headers.Set("Content-Type", "application/json")

		resp, err := c.doRequest(ctx, reqURL, http.MethodGet, nil, headers, false)
		if err != nil {
			return nil, err
		}

		var payload struct {
			Assets []struct {
				AssetID string `json:"assetId"`
			} `json:"assets"`
			NextPageToken string `json:"nextPageToken"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		resp.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}

		for _, item := range payload.Assets {
			id := strings.TrimSpace(item.AssetID)
			if id == "" {
				continue
			}
			assetIDs = append(assetIDs, id)
		}
		nextPageToken = strings.TrimSpace(payload.NextPageToken)
		if nextPageToken == "" {
			break
		}
	}

	return assetIDs, nil
}

func (c *Client) countAssets(ctx context.Context, token string) (int, error) {
	ids, err := c.listAssets(ctx, token)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (c *Client) deleteAsset(ctx context.Context, token string, assetID string) error {
	token = strings.TrimSpace(token)
	assetID = strings.TrimSpace(assetID)
	if NormalizeSSOToken(token) == "" {
		return fmt.Errorf("empty token")
	}
	if assetID == "" {
		return fmt.Errorf("empty asset id")
	}

	reqURL := c.baseURL() + defaultAssetsDeletePath + "/" + url.PathEscape(assetID)
	headers := c.headers(token)
	headers.Set("Origin", "https://grok.com")
	headers.Set("Referer", "https://grok.com/files")
	headers.Set("Content-Type", "application/json")

	resp, err := c.doRequest(ctx, reqURL, http.MethodDelete, nil, headers, false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	return nil
}

func (c *Client) clearAssets(ctx context.Context, token string) (total int, success int, failed int, err error) {
	assetIDs, err := c.listAssets(ctx, token)
	if err != nil {
		return 0, 0, 0, err
	}
	var successes atomic.Int64
	runWorkerPool(ctx, assetIDs, 8, func(id string) {
		if c.deleteAsset(ctx, token, id) == nil {
			successes.Add(1)
		}
	}, nil)
	total, success = len(assetIDs), int(successes.Load())
	return total, success, total - success, nil
}

func grpcWebEncode(payload []byte) []byte {
	out := make([]byte, 5+len(payload))
	out[0] = 0x00
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[5:], payload)
	return out
}

func parseGRPCStatus(headers http.Header, body []byte) (int, string) {
	if headers != nil {
		rawStatus := strings.TrimSpace(firstHeaderValue(headers, "grpc-status", "Grpc-Status"))
		if rawStatus != "" {
			if code, err := strconv.Atoi(rawStatus); err == nil {
				return code, strings.TrimSpace(firstHeaderValue(headers, "grpc-message", "Grpc-Message"))
			}
		}
	}

	buf := body
	for len(buf) >= 5 {
		flag := buf[0]
		frameLen := int(binary.BigEndian.Uint32(buf[1:5]))
		buf = buf[5:]
		if frameLen < 0 || frameLen > len(buf) {
			break
		}
		frame := buf[:frameLen]
		buf = buf[frameLen:]

		// trailer frame
		if flag&0x80 == 0 {
			continue
		}
		lines := strings.Split(string(frame), "\r\n")
		code := -1
		msg := ""
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			kv := strings.SplitN(line, ":", 2)
			if len(kv) != 2 {
				continue
			}
			k := strings.ToLower(strings.TrimSpace(kv[0]))
			v := strings.TrimSpace(kv[1])
			switch k {
			case "grpc-status":
				if n, err := strconv.Atoi(v); err == nil {
					code = n
				}
			case "grpc-message":
				msg = v
			}
		}
		if code >= 0 {
			return code, msg
		}
	}

	// If grpc-status is absent, treat as success by default.
	return -1, ""
}

func (c *Client) grpcWebHeaders(token, origin, referer string) http.Header {
	headers := c.headers(token)
	headers.Set("Origin", origin)
	headers.Set("Referer", referer)
	headers.Set("Content-Type", "application/grpc-web+proto")
	headers.Set("Accept", "*/*")
	headers.Set("Sec-Fetch-Dest", "empty")
	headers.Set("x-grpc-web", "1")
	headers.Set("x-user-agent", "connect-es/2.1.1")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	return headers
}

// readGRPCStatus reads a bounded grpc-web response body and returns the HTTP
// status, grpc status/message, and a descriptive error when the grpc status is
// non-zero.
func readGRPCStatus(resp *http.Response, action string) (int, int, string, error) {
	statusCode := resp.StatusCode
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	grpcStatus, grpcMsg := parseGRPCStatus(resp.Header, raw)
	if grpcStatus != -1 && grpcStatus != 0 {
		if grpcMsg == "" {
			return statusCode, grpcStatus, grpcMsg, fmt.Errorf("%s failed grpc_status=%d", action, grpcStatus)
		}
		return statusCode, grpcStatus, grpcMsg, fmt.Errorf("%s failed grpc_status=%d message=%s", action, grpcStatus, grpcMsg)
	}
	return statusCode, grpcStatus, grpcMsg, nil
}

func (c *Client) acceptTOS(ctx context.Context, token string) (int, error) {
	headers := c.grpcWebHeaders(token, "https://accounts.x.ai", "https://accounts.x.ai/accept-tos")

	payload := grpcWebEncode([]byte{0x10, 0x01})
	resp, err := c.doRequest(ctx, defaultAcceptTOSURL, http.MethodPost, payload, headers, false)
	if err != nil {
		return parseUpstreamStatus(err), err
	}
	defer resp.Body.Close()

	statusCode, _, _, err := readGRPCStatus(resp, "accept tos")
	return statusCode, err
}

func randomAdultBirthDate() string {
	now := time.Now().UTC()
	year := now.Year() - (20 + rand.Intn(29)) // [20,48]
	month := 1 + rand.Intn(12)
	day := 1 + rand.Intn(28)
	hour := rand.Intn(24)
	minute := rand.Intn(60)
	second := rand.Intn(60)
	millis := rand.Intn(1000)
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d.%03dZ", year, month, day, hour, minute, second, millis)
}

func (c *Client) setBirthDate(ctx context.Context, token string) (int, error) {
	payload := map[string]string{
		"birthDate": randomAdultBirthDate(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	headers := c.headers(token)
	headers.Set("Origin", "https://grok.com")
	headers.Set("Referer", "https://grok.com/?_s=home")
	headers.Set("Content-Type", "application/json")

	reqURL := c.baseURL() + defaultSetBirthPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, raw, headers, false)
	if err != nil {
		statusCode := parseUpstreamStatus(err)
		// Upstream may return 204 No Content when already set.
		if statusCode == http.StatusNoContent {
			return statusCode, nil
		}
		return statusCode, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return resp.StatusCode, nil
}

func (c *Client) enableNSFWFeature(ctx context.Context, token string) (int, int, string, error) {
	headers := c.grpcWebHeaders(token, "https://grok.com", "https://grok.com/?_s=data")

	name := []byte("always_show_nsfw_content")
	inner := append([]byte{0x0a, byte(len(name))}, name...)
	protobuf := append([]byte{0x0a, 0x02, 0x10, 0x01, 0x12, byte(len(inner))}, inner...)
	payload := grpcWebEncode(protobuf)

	reqURL := c.baseURL() + defaultNSFWMgmtPath
	resp, err := c.doRequest(ctx, reqURL, http.MethodPost, payload, headers, false)
	if err != nil {
		return parseUpstreamStatus(err), -1, "", err
	}
	defer resp.Body.Close()

	return readGRPCStatus(resp, "enable nsfw")
}

func (c *Client) EnableNSFWDetailed(ctx context.Context, token string) NSFWEnableResult {
	result := NSFWEnableResult{
		Success:    false,
		HTTPStatus: 0,
		GRPCStatus: -1,
	}
	if strings.TrimSpace(token) == "" {
		result.Error = "empty token"
		return result
	}

	statusCode, err := c.acceptTOS(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if err != nil {
		result.Error = fmt.Sprintf("Accept ToS failed: %v", err)
		return result
	}

	statusCode, err = c.setBirthDate(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	if err != nil {
		result.Error = fmt.Sprintf("Set birth date failed: %v", err)
		return result
	}

	statusCode, grpcStatus, grpcMsg, err := c.enableNSFWFeature(ctx, token)
	if statusCode > 0 {
		result.HTTPStatus = statusCode
	}
	result.GRPCStatus = grpcStatus
	result.GRPCMessage = grpcMsg
	if err != nil {
		result.Error = fmt.Sprintf("NSFW enable failed: %v", err)
		return result
	}

	result.Success = true
	result.Error = ""
	return result
}

func newHTTPClient(cfg *config.Config, timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	proxyKey := "direct"
	if cfg != nil {
		proxyKey = httpclient.GenerateProxyKeyFromConfig(cfg)
	}

	return httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(proxyKey, timeout, 0, proxyFunc)
}

func parseRetryAfter(raw string) time.Duration {
	return parseRetryAfterHeader(raw, time.Now())
}

func parseUpstreamStatus(err error) int {
	if err == nil {
		return 0
	}
	raw := err.Error()
	idx := strings.Index(raw, "status=")
	if idx < 0 {
		return 0
	}
	rest := raw[idx+len("status="):]
	n := 0
	for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
		n++
	}
	if n == 0 {
		return 0
	}
	code, convErr := strconv.Atoi(rest[:n])
	if convErr != nil {
		return 0
	}
	return code
}

func backoffDelay(baseDelay, retry429Delay, lastDelay time.Duration, attempt int, status int, retryAfter time.Duration) time.Duration {
	maxDelay := 30 * time.Second
	if retryAfter > 0 {
		if retryAfter > maxDelay {
			return maxDelay
		}
		return retryAfter
	}

	if status == http.StatusTooManyRequests {
		if retry429Delay > 0 {
			if retry429Delay > maxDelay {
				return maxDelay
			}
			return retry429Delay
		}
		if lastDelay <= 0 {
			lastDelay = baseDelay
		}
		upper := lastDelay * 3
		if upper < baseDelay {
			upper = baseDelay
		}
		delay := baseDelay + time.Duration(rand.Int63n(int64(upper-baseDelay)+1))
		if delay > maxDelay {
			return maxDelay
		}
		return delay
	}

	if baseDelay <= 0 {
		baseDelay = 500 * time.Millisecond
	}
	exp := baseDelay * time.Duration(1<<max(0, attempt))
	if exp > maxDelay {
		exp = maxDelay
	}
	if exp <= 0 {
		return baseDelay
	}
	return time.Duration(rand.Int63n(int64(exp) + 1))
}
