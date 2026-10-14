package cline

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/util"
)

// The Cline login is the standard WorkOS device authorization grant, followed by
// an exchange at api.cline.bot:
//
//	1. the server asks WorkOS for a device code and a user code,
//	2. the operator opens the official page and confirms the user code,
//	3. the server polls WorkOS until the grant is issued,
//	4. the WorkOS tokens are exchanged at /auth/register for the Cline pair.
//
// The bridge never sees the operator's password, and only the Cline pair is
// persisted: the WorkOS grant exists solely to obtain it.

// authRequestTimeout bounds a single control-plane request.
const authRequestTimeout = 30 * time.Second

// LoginTTL bounds one authorization transaction. WorkOS states its own expiry;
// this is the ceiling when it does not.
const LoginTTL = 15 * time.Minute

// LoginTransaction is one in-flight device authorization.
type LoginTransaction struct {
	// DeviceCode is the half that can be exchanged. It never leaves the server:
	// the console receives the user code and the page, and polls by id.
	DeviceCode string
	// UserCode is the short code the operator confirms on the official page. It
	// is safe to display: without the device code it cannot be exchanged.
	UserCode string
	// VerifyURL is the human-facing page and VerifyFull the same page with the
	// code embedded.
	VerifyURL  string
	VerifyFull string
	// Interval is the upstream's poll cadence.
	Interval time.Duration
	// ExpiresAt bounds polling.
	ExpiresAt time.Time
}

// StartLogin mints one device authorization transaction.
func (c *Client) StartLogin(ctx context.Context) (*LoginTransaction, error) {
	if c == nil {
		return nil, fmt.Errorf("cline client is nil")
	}
	form := url.Values{}
	form.Set("client_id", c.workOSClientID)

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.workOSAuthorizeURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build device authorize request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", ErrAuthRejected, apiError(http.MethodPost, c.workOSAuthorizeURL, resp.StatusCode, raw))
	}

	var payload struct {
		DeviceCode    string `json:"device_code"`
		UserCode      string `json:"user_code"`
		VerifyURI     string `json:"verification_uri"`
		VerifyURIComp string `json:"verification_uri_complete"`
		Interval      int    `json:"interval"`
		ExpiresIn     int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode device authorize response: %w", err)
	}
	if strings.TrimSpace(payload.DeviceCode) == "" {
		return nil, fmt.Errorf("%w: device authorize response carried no device code", ErrAuthRejected)
	}

	// Only an allowed authorization host may ever be handed to a browser: this
	// is the one place a login could be redirected to a third party.
	page := util.FirstNonEmpty(payload.VerifyURIComp, payload.VerifyURI)
	if host := util.HostOf(page); !allowedLoginHost(host, util.HostOf(c.workOSAuthorizeURL)) {
		return nil, fmt.Errorf("%w: authorization host %q is not an allowed Cline host", ErrAuthRejected, host)
	}

	interval := time.Duration(payload.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(payload.ExpiresIn) * time.Second
	if expiresIn <= 0 || expiresIn > LoginTTL {
		expiresIn = LoginTTL
	}

	return &LoginTransaction{
		DeviceCode: strings.TrimSpace(payload.DeviceCode),
		UserCode:   strings.TrimSpace(payload.UserCode),
		VerifyURL:  strings.TrimSpace(payload.VerifyURI),
		VerifyFull: strings.TrimSpace(payload.VerifyURIComp),
		Interval:   interval,
		ExpiresAt:  time.Now().Add(expiresIn),
	}, nil
}

// PollLogin exchanges one authorization attempt for the Cline credential pair.
//
// WorkOS reports a pending authorization as a non-2xx status carrying
// `error=authorization_pending`, which is a normal step of the flow; ErrAuthPending
// is returned so the caller keeps polling. `slow_down` is honoured by the
// caller's cadence rather than by sleeping here.
func (c *Client) PollLogin(ctx context.Context, tx *LoginTransaction) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("cline client is nil")
	}
	if tx == nil || strings.TrimSpace(tx.DeviceCode) == "" {
		return Credentials{}, fmt.Errorf("cline login transaction is missing")
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	form.Set("device_code", tx.DeviceCode)
	form.Set("client_id", c.workOSClientID)

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.workOSTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, fmt.Errorf("build device authenticate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}

	// A pending authorization is not an error. The body names it, and it is read
	// before the status is trusted: the endpoint is not consistent about which
	// status it pairs with the pending verdict.
	var pending struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(raw, &pending)
	switch strings.ToLower(strings.TrimSpace(pending.Error)) {
	case "authorization_pending", "slow_down":
		return Credentials{}, ErrAuthPending
	}
	if resp.StatusCode != http.StatusOK {
		if strings.Contains(strings.ToLower(string(raw)), "authorization_pending") ||
			strings.Contains(strings.ToLower(string(raw)), "slow_down") {
			return Credentials{}, ErrAuthPending
		}
		return Credentials{}, apiError(http.MethodPost, c.workOSTokenURL, resp.StatusCode, raw)
	}

	var granted struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &granted); err != nil {
		return Credentials{}, fmt.Errorf("decode device authenticate response: %w", err)
	}
	if strings.TrimSpace(granted.AccessToken) == "" {
		return Credentials{}, fmt.Errorf("%w: authenticate response carried no access token", ErrAuthRejected)
	}
	return c.Register(ctx, granted.AccessToken, granted.RefreshToken)
}

// Register exchanges a WorkOS grant for the Cline credential pair.
func (c *Client) Register(ctx context.Context, workosAccess, workosRefresh string) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("cline client is nil")
	}
	workosAccess = strings.TrimSpace(workosAccess)
	if workosAccess == "" {
		return Credentials{}, ErrCredentialMissing
	}
	body, err := json.Marshal(map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": strings.TrimSpace(workosRefresh),
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("marshal register request: %w", err)
	}
	endpoint := c.apiBase + "/auth/register"

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Credentials{}, fmt.Errorf("build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, apiError(http.MethodPost, endpoint, resp.StatusCode, raw)
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
			UserInfo     *struct {
				Email string `json:"email"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Credentials{}, fmt.Errorf("decode register response: %w", err)
	}
	if strings.TrimSpace(payload.Data.AccessToken) == "" {
		return Credentials{}, fmt.Errorf("%w: register response carried no access token", ErrAuthRejected)
	}
	creds := Credentials{
		AccessToken:  strings.TrimSpace(payload.Data.AccessToken),
		RefreshToken: strings.TrimSpace(payload.Data.RefreshToken),
		ExpiresAt:    ParseExpiry(payload.Data.ExpiresAt),
	}
	if payload.Data.UserInfo != nil {
		creds.Email = strings.TrimSpace(payload.Data.UserInfo.Email)
	}
	return creds, nil
}

// Refresh renews the Cline credential from the durable refresh token.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("cline client is nil")
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return Credentials{}, ErrCredentialMissing
	}
	body, err := json.Marshal(map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("marshal refresh request: %w", err)
	}
	endpoint := c.apiBase + "/auth/refresh"

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Credentials{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		err := apiError(http.MethodPost, endpoint, resp.StatusCode, raw)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// The refresh grant itself was refused: no local retry can recover
			// it, and the operator has to authorize again.
			return Credentials{}, fmt.Errorf("%w: %v", ErrReLoginRequired, err)
		}
		return Credentials{}, err
	}

	var payload struct {
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    any    `json:"expiresAt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Credentials{}, fmt.Errorf("decode refresh response: %w", err)
	}
	if strings.TrimSpace(payload.Data.AccessToken) == "" {
		return Credentials{}, fmt.Errorf("%w: refresh response carried no access token", ErrReLoginRequired)
	}
	return Credentials{
		AccessToken:  strings.TrimSpace(payload.Data.AccessToken),
		RefreshToken: util.FirstNonEmptyUntrimmed(payload.Data.RefreshToken, refreshToken),
		ExpiresAt:    ParseExpiry(payload.Data.ExpiresAt),
	}, nil
}

// apiError renders an upstream failure without echoing the credential.
func apiError(method, rawURL string, status int, raw []byte) error {
	parts := []string{
		fmt.Sprintf("status=%d", status),
		fmt.Sprintf("method=%s", method),
		fmt.Sprintf("path=%s", util.URLPath(rawURL)),
	}
	if body := strings.TrimSpace(string(raw)); body != "" {
		parts = append(parts, "message="+util.Truncate(body, 300))
	}
	return fmt.Errorf("cline API error: %s", strings.Join(parts, ", "))
}
