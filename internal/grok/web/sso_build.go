package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"

	"orchids-api/internal/config"
)

// A Web SSO cookie is a signed-in xAI browser session, and the Build CLI's
// device authorization is approved from exactly such a session. This file
// performs the browser half of that flow on the account's behalf: it submits
// the user code, reads the consent page and approves it. The device and token
// requests themselves stay with the grok package's DeviceAuthenticator.

const (
	ssoDeviceVerifyURL  = "https://auth.x.ai/oauth2/device/verify"
	ssoDeviceApproveURL = "https://auth.x.ai/oauth2/device/approve"
	maxSSOAuthBody      = 2 << 20
)

var (
	// ErrSSOSessionRejected reports that xAI sent the browser session to its
	// sign-in page: the SSO cookie is not a live session.
	ErrSSOSessionRejected = errors.New("xAI did not accept the SSO session")
	// ErrBrowserVerificationRequired reports that xAI refused the automated
	// approval because it wants an interactive browser check. The cookie is
	// fine; the device code has to be approved by hand.
	ErrBrowserVerificationRequired = errors.New("xAI requires browser verification to approve the device authorization")
)

// ssoAuthStatusError carries only the HTTP status of a failed step. The body
// of an auth page can echo codes and tokens, so it is never attached.
type ssoAuthStatusError struct {
	step   string
	status int
}

func (e *ssoAuthStatusError) Error() string {
	return fmt.Sprintf("xAI device authorization %s returned HTTP %d", e.step, e.status)
}

type ssoBuildFlow struct {
	client    *http.Client
	userAgent string
	cookies   map[string]string
}

// ApproveDeviceCode approves a Build device authorization with a Web account's
// SSO session. userCode and deviceCode come from the device authorization the
// caller started; on success the caller's token poll completes.
func ApproveDeviceCode(ctx context.Context, cfg *config.Config, ssoToken, userCode, deviceCode string) error {
	ssoToken = NormalizeSSOToken(ssoToken)
	userCode = strings.TrimSpace(userCode)
	if ssoToken == "" {
		return ErrMissingSSOToken
	}
	if userCode == "" {
		return errors.New("missing device user code")
	}
	client := New(cfg)
	httpClient := *client.clientForAppChat()
	// Redirects are followed by hand: each hop must stay on x.ai, and the flow
	// has to observe where a step lands rather than what that page renders.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	httpClient.Jar = nil
	cfClearance, cfBM := client.cloudflareCookies()
	flow := &ssoBuildFlow{client: &httpClient, userAgent: client.userAgent(), cookies: map[string]string{}}
	for _, item := range grokCookieItems(buildGrokCookie(ssoToken, cfClearance, cfBM)) {
		if item.name != "" {
			flow.cookies[item.name] = item.value
		}
	}
	return flow.approve(ctx, userCode, deviceCode)
}

func (f *ssoBuildFlow) approve(ctx context.Context, userCode, deviceCode string) error {
	// verify → consent. The consent page embeds a one-time consent_token (an
	// ES256 JWT, typ=consent+jwt) that approve must send back.
	status, landed, body, err := f.do(ctx, http.MethodPost, ssoDeviceVerifyURL, url.Values{"user_code": {userCode}}, false, nil)
	if err != nil {
		return err
	}
	if status == http.StatusUnauthorized {
		return ErrSSOSessionRejected
	}
	if status < 200 || status >= 400 {
		return &ssoAuthStatusError{step: "verify", status: status}
	}
	switch ssoDeviceRedirectState(landed) {
	case "consent":
	case "sign-in":
		return ErrSSOSessionRejected
	default:
		return errors.New("xAI device authorization verify did not reach the consent page")
	}
	consentURL := landed

	// A 3xx to the consent page has an empty body: fetch the page itself.
	consentToken := extractConsentInput(body, "consent_token")
	pageUserCode := extractConsentInput(body, "user_code")
	if consentToken == "" {
		status, _, body, err = f.do(ctx, http.MethodGet, consentURL, nil, true, nil)
		if err != nil {
			return fmt.Errorf("read the xAI consent page: %w", err)
		}
		if status < 200 || status >= 300 {
			return &ssoAuthStatusError{step: "consent", status: status}
		}
		consentToken = extractConsentToken(body)
		pageUserCode = extractConsentInput(body, "user_code")
	}
	if !consentTokenMatchesDevice(consentToken, deviceCode) && !consentTokenMatchesUserCode(consentToken, pageUserCode, userCode, time.Now()) {
		return errors.New("the xAI consent page carried no consent token for this device authorization")
	}

	status, landed, body, err = f.do(ctx, http.MethodPost, ssoDeviceApproveURL, url.Values{
		"user_code": {userCode}, "action": {"allow"}, "principal_type": {"User"}, "principal_id": {""},
		"consent_token": {consentToken},
	}, false, map[string]string{
		"Origin":                    "https://accounts.x.ai",
		"Referer":                   consentURL,
		"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "same-site",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	})
	if err != nil {
		return err
	}
	if status == http.StatusForbidden && isBrowserVerificationRequired(body) {
		return ErrBrowserVerificationRequired
	}
	if status < 200 || status >= 400 {
		return &ssoAuthStatusError{step: "approve", status: status}
	}
	switch ssoDeviceRedirectState(landed) {
	case "done":
		return nil
	case "sign-in":
		return ErrSSOSessionRejected
	default:
		return errors.New("xAI device authorization approve did not complete")
	}
}

// do sends one step. With follow=false a 3xx is returned at once with the
// resolved Location as the landing URL; the redirect target (accounts.x.ai) is
// inspected, not rendered.
func (f *ssoBuildFlow) do(ctx context.Context, method, endpoint string, form url.Values, follow bool, extraHeaders map[string]string) (int, string, []byte, error) {
	if !safeXAIURL(endpoint) {
		return 0, "", nil, errors.New("xAI authorization URL is not trusted")
	}
	currentURL, currentMethod, currentForm := endpoint, method, form
	for redirects := 0; redirects <= 8; redirects++ {
		var body io.Reader
		if currentForm != nil {
			body = strings.NewReader(currentForm.Encode())
		}
		request, err := http.NewRequestWithContext(ctx, currentMethod, currentURL, body)
		if err != nil {
			return 0, "", nil, err
		}
		request.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
		request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		request.Header.Set("User-Agent", f.userAgent)
		request.Header.Set("Cookie", f.cookieHeader())
		if currentMethod == http.MethodGet && ssoDeviceRedirectState(currentURL) == "consent" {
			request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
			request.Header.Set("Referer", "https://auth.x.ai/")
			request.Header.Set("Sec-Fetch-Dest", "document")
			request.Header.Set("Sec-Fetch-Mode", "navigate")
			request.Header.Set("Sec-Fetch-Site", "same-site")
			request.Header.Set("Upgrade-Insecure-Requests", "1")
		}
		for key, value := range extraHeaders {
			request.Header.Set(key, value)
		}
		if currentForm != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		response, err := f.client.Do(request)
		if err != nil {
			return 0, "", nil, err
		}
		f.captureCookies(response)
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxSSOAuthBody+1))
		_ = response.Body.Close()
		if readErr != nil {
			return response.StatusCode, currentURL, nil, readErr
		}
		if len(data) > maxSSOAuthBody {
			return response.StatusCode, currentURL, nil, errors.New("xAI authorization response is too large")
		}
		if response.StatusCode < 300 || response.StatusCode > 399 {
			return response.StatusCode, currentURL, data, nil
		}
		location := strings.TrimSpace(response.Header.Get("Location"))
		if location == "" {
			return response.StatusCode, currentURL, data, errors.New("xAI authorization redirect carried no Location")
		}
		base, _ := url.Parse(currentURL)
		next, err := url.Parse(location)
		if err != nil {
			return response.StatusCode, currentURL, data, errors.New("xAI authorization redirect is malformed")
		}
		currentURL = base.ResolveReference(next).String()
		if !safeXAIURL(currentURL) {
			return response.StatusCode, currentURL, data, errors.New("xAI authorization redirected off x.ai")
		}
		if !follow {
			return response.StatusCode, currentURL, data, nil
		}
		if response.StatusCode == http.StatusSeeOther || ((response.StatusCode == http.StatusMovedPermanently || response.StatusCode == http.StatusFound) && currentMethod != http.MethodGet && currentMethod != http.MethodHead) {
			currentMethod, currentForm = http.MethodGet, nil
		}
	}
	return 0, currentURL, nil, errors.New("xAI authorization redirected too many times")
}

func (f *ssoBuildFlow) captureCookies(response *http.Response) {
	for _, cookie := range response.Cookies() {
		name, value := strings.TrimSpace(cookie.Name), strings.TrimSpace(cookie.Value)
		if name == "" || len(name) > 128 || len(value) > 16384 || strings.ContainsAny(name+value, "\r\n\x00") {
			continue
		}
		if cookie.MaxAge < 0 {
			delete(f.cookies, name)
			continue
		}
		f.cookies[name] = value
	}
}

func (f *ssoBuildFlow) cookieHeader() string {
	keys := make([]string, 0, len(f.cookies))
	for key := range f.cookies {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+f.cookies[key])
	}
	return strings.Join(parts, "; ")
}

// ssoDeviceRedirectState names the step of the device flow a URL belongs to.
func ssoDeviceRedirectState(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || !safeXAIURL(raw) {
		return ""
	}
	switch strings.TrimRight(parsed.EscapedPath(), "/") {
	case "/oauth2/device/consent":
		return "consent"
	case "/oauth2/device/done":
		return "done"
	case "/sign-in", "/sign-up":
		return "sign-in"
	default:
		return ""
	}
}

// safeXAIURL accepts only https URLs on x.ai without userinfo: the flow sends
// the SSO cookie to every URL it visits.
func safeXAIURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

var consentTokenJSONRe = regexp.MustCompile(`(?i)\\?["']?(?:consentToken|consent_token)\\?["']?\s*[:=]\s*\\?["'](eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\\?["']`)

// extractConsentToken reads the one-time consent token from the consent page:
// the hidden form field first, then the page's embedded JSON.
func extractConsentToken(body []byte) string {
	if value := extractConsentInput(body, "consent_token"); value != "" {
		return value
	}
	if m := consentTokenJSONRe.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	return ""
}

// extractConsentInput returns the value of a hidden input on the consent form.
func extractConsentInput(body []byte, field string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(string(body)))
	for {
		tokenType := tokenizer.Next()
		if tokenType == html.ErrorToken {
			return ""
		}
		if tokenType != html.StartTagToken && tokenType != html.SelfClosingTagToken {
			continue
		}
		token := tokenizer.Token()
		if !strings.EqualFold(token.Data, "input") {
			continue
		}
		var name, value string
		for _, attribute := range token.Attr {
			switch strings.ToLower(attribute.Key) {
			case "name":
				name = attribute.Val
			case "value":
				value = attribute.Val
			}
		}
		if strings.EqualFold(name, field) && value != "" {
			return value
		}
	}
}

func consentClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(data, &claims) != nil {
		return nil
	}
	return claims
}

// consentTokenMatchesDevice accepts a token whose dc claim is our device code.
func consentTokenMatchesDevice(token, deviceCode string) bool {
	dc, _ := consentClaims(token)["dc"].(string)
	return deviceCode != "" && strings.TrimSpace(dc) == deviceCode
}

// consentTokenMatchesUserCode binds a consent token to this device request
// when its dc claim is a server-side authorization id rather than the device
// code. The consent form echoes our user code, so the token is accepted only
// from that form, with the consent audience and an unexpired exp.
func consentTokenMatchesUserCode(token, pageUserCode, userCode string, now time.Time) bool {
	if userCode == "" || !strings.EqualFold(strings.TrimSpace(pageUserCode), userCode) {
		return false
	}
	claims := consentClaims(token)
	if claims == nil {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(strings.SplitN(token, ".", 2)[0])
	if err != nil {
		return false
	}
	var head struct {
		Typ string `json:"typ"`
	}
	if json.Unmarshal(header, &head) != nil || !strings.EqualFold(head.Typ, "consent+jwt") {
		return false
	}
	aud, _ := claims["aud"].(string)
	dc, _ := claims["dc"].(string)
	if aud != "consent" || strings.TrimSpace(dc) == "" {
		return false
	}
	exp, _ := claims["exp"].(float64)
	return exp > 0 && time.Unix(int64(exp), 0).After(now)
}

func isBrowserVerificationRequired(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "could not verify this browser") || strings.Contains(text, "request could not be verified")
}
