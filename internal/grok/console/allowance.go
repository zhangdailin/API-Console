package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// A Console account's allowance is the weekly included-usage pool shown on
// the grok.com dashboard (served by Cursor's WorkOS identity layer). One GET
// of the dashboard login route exchanges the SSO cookie for a WorkOS cookie
// triple, which usage-status then accepts. Inference never needs it.
//
// The WorkOS cookies are a credential, so they are cached in memory only:
// losing them costs one more exchange, never a stored secret.

const (
	workosSessionCookie = "WorkosCursorSessionToken"
	workosOriginCookie  = "WorkosCursorXaiOrigin"
	workosIDCookie      = "workos_id"

	workosLoginPath     = "/dashboard/api/auth/login"
	workosUsagePath     = "/dashboard/api/dashboard/v2/usage-status"
	workosUsageRedirect = "/dashboard/usage"
	workosMaxRedirects  = 12
	workosBodyLimit     = 1 << 20
)

var (
	// ErrUnlinked marks an account never linked to Cursor: it has no Console
	// credit pool, so retrying cannot help.
	ErrUnlinked = errors.New("the Grok account is not linked to Cursor and has no Console credits")
	// ErrCloudflare marks a Cloudflare block or challenge page: an egress
	// problem shared by every account on that exit, not an account verdict.
	ErrCloudflare = errors.New("cloudflare rejected the request")
)

type workosSession struct{ token, origin, id string }

func (s workosSession) complete() bool { return s.token != "" && s.origin != "" && s.id != "" }

func (s workosSession) cookieHeader() string {
	return workosSessionCookie + "=" + s.token + "; " + workosOriginCookie + "=" + s.origin + "; " + workosIDCookie + "=" + s.id
}

// Allowance is one read of the weekly pool.
type Allowance struct {
	BudgetCents int64
	UsedCents   int64
	ResetAt     time.Time
}

func (c *Client) sessionOrigin() string {
	if c != nil && c.cfg != nil {
		return c.cfg.GrokWebBaseURLOrDefault()
	}
	return config.DefaultGrokWebBaseURL
}

func (c *Client) noRedirectClient() *http.Client {
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	return &client
}

func (c *Client) dashboardHeaders(header http.Header) {
	header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	header.Set("Cache-Control", "no-cache")
	header.Set("Pragma", "no-cache")
	header.Set("User-Agent", c.userAgent())
}

// FetchAllowance reads the account's weekly pool, exchanging a WorkOS session
// when none is cached or the cached one is refused.
func (c *Client) FetchAllowance(ctx context.Context, ssoToken string) (Allowance, error) {
	key := sessionKey(ssoToken)
	c.mu.Lock()
	session, cached := c.workos[key]
	c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if !cached {
			fresh, err := c.loginWorkOS(ctx, ssoToken)
			if err != nil {
				return Allowance{}, err
			}
			session = fresh
			c.mu.Lock()
			c.workos[key] = session
			c.mu.Unlock()
		}
		status, data, err := c.requestUsage(ctx, ssoToken, session)
		if err != nil {
			return Allowance{}, err
		}
		switch {
		case status >= 200 && status < 300:
			return parseUsageStatus(data)
		case status == http.StatusUnauthorized && cached:
			cached = false
			continue
		case status == http.StatusUnauthorized:
			return Allowance{}, fmt.Errorf("%w: usage-status refused a fresh WorkOS session", ErrUnauthorized)
		case status == http.StatusForbidden && cloudflareRejection(data):
			return Allowance{}, ErrCloudflare
		default:
			return Allowance{}, fmt.Errorf("Console usage-status returned status %d", status)
		}
	}
	return Allowance{}, errors.New("Console usage-status kept refusing the WorkOS session")
}

func (c *Client) requestUsage(ctx context.Context, ssoToken string, session workosSession) (int, []byte, error) {
	origin := c.sessionOrigin()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+workosUsagePath+"?scope=user", nil)
	if err != nil {
		return 0, nil, err
	}
	c.dashboardHeaders(req.Header)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+workosUsageRedirect)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("x-cursor-dashboard-surface", "v2")
	req.Header.Set("Cookie", c.cookieHeader(ssoToken)+"; "+session.cookieHeader())
	resp, err := c.noRedirectClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, workosBodyLimit))
	return resp.StatusCode, data, err
}

// loginWorkOS follows the dashboard login redirects by hand, inside the hosts
// the Grok/Cursor login is known to traverse, and collects the cookie triple.
func (c *Client) loginWorkOS(ctx context.Context, ssoToken string) (workosSession, error) {
	origin := c.sessionOrigin()
	base, err := url.Parse(origin)
	if err != nil || base.Host == "" {
		return workosSession{}, fmt.Errorf("invalid grok.com origin %q", origin)
	}
	jar, _ := cookiejar.New(nil)
	client := c.noRedirectClient()
	current := origin + workosLoginPath + "?redirect_uri=" + url.QueryEscape(origin+workosUsageRedirect)
	for hop := 0; hop <= workosMaxRedirects; hop++ {
		target, err := url.Parse(current)
		if err != nil {
			return workosSession{}, errors.New("the WorkOS login redirected to a malformed URL")
		}
		if !allowedWorkOSHost(target, base.Host) {
			return workosSession{}, fmt.Errorf("the WorkOS login redirected to an unexpected host %s", target.Hostname())
		}
		if cursorHostedSignIn(target) {
			return workosSession{}, c.classifySignIn(ctx, ssoToken)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return workosSession{}, err
		}
		c.dashboardHeaders(req.Header)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		var cookies []string
		if strings.EqualFold(target.Host, base.Host) {
			// The SSO cookie goes to grok.com only, never to a redirect host.
			cookies = append(cookies, c.cookieHeader(ssoToken))
		}
		for _, cookie := range jar.Cookies(target) {
			cookies = append(cookies, cookie.Name+"="+cookie.Value)
		}
		if len(cookies) > 0 {
			req.Header.Set("Cookie", strings.Join(cookies, "; "))
		}
		resp, err := client.Do(req)
		if err != nil {
			return workosSession{}, err
		}
		jar.SetCookies(target, resp.Cookies())
		location, status := resp.Header.Get("Location"), resp.StatusCode
		var body []byte
		if status < 300 || status >= 400 {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, workosBodyLimit))
		}
		_ = resp.Body.Close()
		if session, ok := workosSessionFromJar(jar, base); ok {
			return session, nil
		}
		if status >= 300 && status < 400 && location != "" {
			next, err := target.Parse(location)
			if err != nil {
				return workosSession{}, errors.New("the WorkOS login redirected to a malformed URL")
			}
			current = next.String()
			continue
		}
		switch {
		case status == http.StatusForbidden && (strings.Contains(target.Host, "cursor") || strings.Contains(target.Host, "workos")):
			return workosSession{}, c.classifySignIn(ctx, ssoToken)
		case status == http.StatusForbidden && cloudflareRejection(body):
			return workosSession{}, ErrCloudflare
		case status == http.StatusUnauthorized:
			return workosSession{}, fmt.Errorf("%w: the WorkOS login returned 401", ErrUnauthorized)
		}
		// An expired SSO also ends here (200 without the cookies): ask the
		// session endpoint so a dead cookie reads as a credential verdict.
		if _, _, sessionErr := c.cursorMerged(ctx, ssoToken); errors.Is(sessionErr, ErrUnauthorized) {
			return workosSession{}, sessionErr
		}
		return workosSession{}, fmt.Errorf("the WorkOS login returned no session (status %d)", status)
	}
	return workosSession{}, errors.New("the WorkOS login redirected too many times")
}

// classifySignIn decides what landing on Cursor's sign-in page means: an
// unlinked account has no Console credits; a linked one there is transient.
func (c *Client) classifySignIn(ctx context.Context, ssoToken string) error {
	merged, known, err := c.cursorMerged(ctx, ssoToken)
	if err != nil {
		return fmt.Errorf("the WorkOS login reached Cursor's sign-in page and the link state could not be read: %w", err)
	}
	if known && merged {
		return errors.New("the WorkOS login reached Cursor's sign-in page for a linked account")
	}
	return ErrUnlinked
}

// cursorMerged reads session.cursorMerged from grok.com's session endpoint.
// known is false when the field is absent (accounts older than the link).
func (c *Client) cursorMerged(ctx context.Context, ssoToken string) (merged, known bool, err error) {
	origin := c.sessionOrigin()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/auth/session", nil)
	if err != nil {
		return false, false, err
	}
	c.dashboardHeaders(req.Header)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("Cookie", c.cookieHeader(ssoToken))
	resp, err := c.noRedirectClient().Do(req)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, workosBodyLimit))
	if err != nil {
		return false, false, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return false, false, ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, false, fmt.Errorf("the grok.com session endpoint returned status %d", resp.StatusCode)
	}
	var payload struct {
		Status  string `json:"status"`
		Session struct {
			CursorMerged *bool `json:"cursorMerged"`
		} `json:"session"`
		CursorMerged *bool `json:"cursorMerged"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return false, false, errors.New("the grok.com session endpoint did not answer JSON")
	}
	if strings.EqualFold(strings.TrimSpace(payload.Status), "unauthenticated") {
		return false, false, ErrUnauthorized
	}
	switch {
	case payload.Session.CursorMerged != nil:
		return *payload.Session.CursorMerged, true, nil
	case payload.CursorMerged != nil:
		return *payload.CursorMerged, true, nil
	}
	return false, false, nil
}

func workosSessionFromJar(jar http.CookieJar, base *url.URL) (workosSession, bool) {
	// The cookies are set on the dashboard path: ask for that path.
	probe := *base
	probe.Path, probe.RawQuery = workosUsagePath, ""
	var session workosSession
	for _, cookie := range jar.Cookies(&probe) {
		switch cookie.Name {
		case workosSessionCookie:
			session.token = cookie.Value
		case workosOriginCookie:
			session.origin = cookie.Value
		case workosIDCookie:
			session.id = cookie.Value
		}
	}
	return session, session.complete()
}

// allowedWorkOSHost bounds the redirect chain so a Location cannot steer the
// session to an arbitrary origin.
func allowedWorkOSHost(target *url.URL, sessionHost string) bool {
	if strings.EqualFold(target.Host, sessionHost) {
		return target.Scheme == "https" || target.Scheme == "http"
	}
	if target.Scheme != "https" {
		return false
	}
	host := strings.ToLower(target.Hostname())
	for _, suffix := range []string{"grok.com", "x.ai", "cursor.com", "cursor.sh", "workos.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func cursorHostedSignIn(target *url.URL) bool {
	host := strings.ToLower(target.Hostname())
	return host == "authenticator.cursor.sh" || ((host == "cursor.com" || host == "www.cursor.com") && strings.HasPrefix(target.Path, "/unify"))
}

func cloudflareRejection(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "attention required! | cloudflare") || strings.Contains(text, "cf-error-details") ||
		strings.Contains(text, "just a moment") || strings.Contains(text, "cf-challenge") || strings.Contains(text, "challenge-platform")
}

func parseUsageStatus(data []byte) (Allowance, error) {
	var payload struct {
		Weekly map[string]json.RawMessage `json:"weeklyIncludedUsage"`
	}
	if err := json.Unmarshal(data, &payload); err != nil || payload.Weekly == nil {
		return Allowance{}, errors.New("Console usage-status has no weeklyIncludedUsage")
	}
	budget, ok := jsonInteger(payload.Weekly["budgetCents"])
	if !ok || budget < 0 {
		return Allowance{}, errors.New("Console usage-status has no valid budgetCents")
	}
	// proto3 JSON omits zero values: a missing usedCents is 0.
	used, _ := jsonInteger(payload.Weekly["usedCents"])
	if used < 0 {
		return Allowance{}, errors.New("Console usage-status has a negative usedCents")
	}
	allowance := Allowance{BudgetCents: budget, UsedCents: used}
	allowance.ResetAt, _ = jsonTimestamp(payload.Weekly["resetsAt"])
	return allowance, nil
}

// jsonInteger accepts JSON numbers and the decimal strings proto3 JSON uses
// for 64-bit integers.
func jsonInteger(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, false
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		if value, err := number.Int64(); err == nil {
			return value, true
		}
		if value, err := number.Float64(); err == nil && !math.IsInf(value, 0) {
			return int64(value), true
		}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
			return value, true
		}
	}
	return 0, false
}

// jsonTimestamp accepts RFC 3339 strings, proto Timestamp objects and Unix
// seconds or milliseconds.
func jsonTimestamp(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text)); err == nil {
			return parsed.UTC(), true
		}
		raw = json.RawMessage(strings.TrimSpace(text))
	}
	var object struct {
		Seconds json.RawMessage `json:"seconds"`
	}
	if json.Unmarshal(raw, &object) == nil && len(object.Seconds) > 0 {
		if seconds, ok := jsonInteger(object.Seconds); ok && seconds > 0 {
			return time.Unix(seconds, 0).UTC(), true
		}
	}
	if value, ok := jsonInteger(raw); ok && value > 0 {
		if value > 1_000_000_000_000 {
			return time.UnixMilli(value).UTC(), true
		}
		return time.Unix(value, 0).UTC(), true
	}
	return time.Time{}, false
}

// ApplyAllowance records a read on the account.
func ApplyAllowance(acc *store.Account, allowance Allowance, now time.Time) {
	acc.GrokConsoleQuota = store.GrokConsoleQuotaSnapshot{
		BudgetCents: allowance.BudgetCents, UsedCents: allowance.UsedCents, ResetAt: allowance.ResetAt, SyncedAt: now.UTC(),
	}
}

// ApplyUnlinked records that the account has no Console credit pool.
func ApplyUnlinked(acc *store.Account, now time.Time) {
	acc.GrokConsoleQuota = store.GrokConsoleQuotaSnapshot{Unlinked: true, SyncedAt: now.UTC()}
}
