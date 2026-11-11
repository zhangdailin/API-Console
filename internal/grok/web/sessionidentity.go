package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/httpclient"
	"strings"
	"time"
)

// AccountIdentity carries the stable identity resolved from Grok's session
// endpoint for an SSO account. TeamID feeds team+model granularity rate
// limiting and account deduplication.
type AccountIdentity struct {
	UserID string
	Email  string
	TeamID string
}

// ParseSessionIdentity parses the Grok /api/auth/session response. It rejects
// unavailable sessions (unauthenticated/blocked) before accepting residual
// identity fields, and falls back across multiple JSON shapes.
func ParseSessionIdentity(body []byte) (AccountIdentity, error) {
	var value struct {
		Status  string `json:"status"`
		Session struct {
			UserID         string `json:"userId"`
			Email          string `json:"email"`
			OrganizationID string `json:"organizationId"`
		} `json:"session"`
		User struct {
			ID     string `json:"id"`
			UserID string `json:"userId"`
			Sub    string `json:"sub"`
			Email  string `json:"email"`
			TeamID string `json:"teamId"`
		} `json:"user"`
		ID     string `json:"id"`
		UserID string `json:"userId"`
		Sub    string `json:"sub"`
		Email  string `json:"email"`
		TeamID string `json:"teamId"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return AccountIdentity{}, fmt.Errorf("parse grok session: %w", err)
	}
	status := strings.ToLower(strings.TrimSpace(value.Status))
	if status == "unauthenticated" {
		return AccountIdentity{}, errGrokSessionUnauthenticated
	}
	if status == "blocked" {
		return AccountIdentity{}, errGrokSessionBlocked
	}
	identity := AccountIdentity{
		UserID: firstNonEmpty(value.Session.UserID, value.User.ID, value.User.UserID, value.User.Sub, value.ID, value.UserID, value.Sub),
		Email:  firstNonEmpty(value.Session.Email, value.User.Email, value.Email),
		TeamID: firstNonEmpty(value.Session.OrganizationID, value.User.TeamID, value.TeamID),
	}
	if identity.UserID == "" && identity.Email == "" {
		return AccountIdentity{}, fmt.Errorf("grok session missing account identity")
	}
	return identity, nil
}

var (
	errGrokSessionUnauthenticated = fmt.Errorf("grok session unauthenticated")
	errGrokSessionBlocked         = fmt.Errorf("grok session blocked")
)

// IsAuthenticationFailure reports only definitive SSO authentication or
// account-block signals. Quota/model availability errors intentionally do not
// match this predicate.
func IsAuthenticationFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errGrokSessionUnauthenticated) || errors.Is(err, errGrokSessionBlocked) {
		return true
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "status=401") ||
		strings.Contains(lower, "status 401") ||
		strings.Contains(lower, "blocked-user") ||
		strings.Contains(lower, "user is blocked")
}

// FetchSessionIdentity resolves the stable identity of an SSO account via
// GET {base}/api/auth/session. 15s timeout; the request uses the same browser
// headers and cookie as app-chat so clearance/binding stays consistent.
func (c *Client) FetchSessionIdentity(ctx context.Context, token string) (AccountIdentity, error) {
	if c == nil {
		return AccountIdentity{}, fmt.Errorf("grok client not configured")
	}
	if NormalizeSSOToken(token) == "" {
		return AccountIdentity{}, errGrokSessionUnauthenticated
	}
	endpoint := strings.TrimRight(c.baseURL(), "/") + "/api/auth/session"
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return AccountIdentity{}, err
	}
	req.Header = http.Header{
		"Accept":          {"*/*"},
		"Accept-Encoding": {"gzip, deflate, br, zstd"},
		"Accept-Language": {"zh-CN,zh;q=0.9,en;q=0.8"},
		"Cache-Control":   {"no-cache"},
		"Cookie":          {buildGrokCookie(token, "", "")},
		"Pragma":          {"no-cache"},
		"Priority":        {"u=1, i"},
		"Referer":         {strings.TrimRight(c.baseURL(), "/") + "/"},
		"Sec-Fetch-Dest":  {"empty"},
		"Sec-Fetch-Mode":  {"cors"},
		"Sec-Fetch-Site":  {"same-origin"},
		"User-Agent":      {c.userAgent()},
	}
	applyChromiumClientHints(req.Header, c.userAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return AccountIdentity{}, err
	}
	defer resp.Body.Close()
	if err := httpclient.DecodeContentEncoding(resp); err != nil {
		return AccountIdentity{}, fmt.Errorf("decode grok session response: %w", err)
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return AccountIdentity{}, readErr
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return AccountIdentity{}, errGrokSessionUnauthenticated
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return AccountIdentity{}, fmt.Errorf("grok session endpoint returned %d", resp.StatusCode)
	}
	return ParseSessionIdentity(body)
}
