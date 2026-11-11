package cline

import (
	"bytes"
	"context"
	"encoding/base64"
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

// loginTTL bounds one authorization transaction. WorkOS states its own expiry;
// this is the ceiling when it does not, or when it states a longer one.
const loginTTL = 15 * time.Minute

// controlBodyLimit bounds an auth or plan response body.
const controlBodyLimit = 64 << 10

// controlCall performs one control-plane request and returns its status and
// bounded body. A request that got no usable answer is ErrAuthUnavailable.
// creds, when set, adds the bearer credential and the product identity headers.
func (c *Client) controlCall(ctx context.Context, method, endpoint, contentType string, body []byte, creds *Credentials, taskID string, timeout time.Duration, limit int64) (int, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build cline request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if taskID != "" {
		req.Header.Set("X-Task-ID", taskID)
	}
	if creds != nil {
		req.Header.Set("Authorization", "Bearer "+creds.Bearer())
		applyClientHeaders(req.Header)
	}
	resp, raw, err := httpclient.DoReadBody(c.control, req, limit)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	return resp.StatusCode, raw, nil
}

// postForm sends one urlencoded request to a WorkOS endpoint.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	return c.controlCall(ctx, http.MethodPost, endpoint, "application/x-www-form-urlencoded",
		[]byte(form.Encode()), nil, "", authRequestTimeout, controlBodyLimit)
}

// postJSON sends one JSON request to a Cline auth endpoint.
func (c *Client) postJSON(ctx context.Context, endpoint string, fields map[string]string) (int, []byte, error) {
	body, err := json.Marshal(fields)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal cline request: %w", err)
	}
	return c.controlCall(ctx, http.MethodPost, endpoint, "application/json", body, nil, "", authRequestTimeout, controlBodyLimit)
}

// getAuthorized reads one account-scoped resource with the product identity.
func (c *Client) getAuthorized(ctx context.Context, endpoint string, creds Credentials, taskID string, timeout time.Duration, limit int64) (int, []byte, error) {
	return c.controlCall(ctx, http.MethodGet, endpoint, "", nil, &creds, taskID, timeout, limit)
}

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
	// ExpiresAt bounds polling.
	ExpiresAt time.Time
}

// StartLogin mints one device authorization transaction.
func (c *Client) StartLogin(ctx context.Context) (*LoginTransaction, error) {
	if c == nil {
		return nil, fmt.Errorf("cline client is nil")
	}
	status, raw, err := c.postForm(ctx, c.workOSAuthorizeURL, url.Values{"client_id": {c.workOSClientID}})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: %s", ErrAuthRejected, apiError(http.MethodPost, c.workOSAuthorizeURL, status, raw))
	}

	var payload struct {
		DeviceCode    string `json:"device_code"`
		UserCode      string `json:"user_code"`
		VerifyURI     string `json:"verification_uri"`
		VerifyURIComp string `json:"verification_uri_complete"`
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

	expiresIn := time.Duration(payload.ExpiresIn) * time.Second
	if expiresIn <= 0 || expiresIn > loginTTL {
		expiresIn = loginTTL
	}

	return &LoginTransaction{
		DeviceCode: strings.TrimSpace(payload.DeviceCode),
		UserCode:   strings.TrimSpace(payload.UserCode),
		VerifyURL:  strings.TrimSpace(payload.VerifyURI),
		VerifyFull: strings.TrimSpace(payload.VerifyURIComp),
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

	status, raw, err := c.postForm(ctx, c.workOSTokenURL, url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {tx.DeviceCode},
		"client_id":   {c.workOSClientID},
	})
	if err != nil {
		return Credentials{}, err
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
	if status != http.StatusOK {
		if text := strings.ToLower(string(raw)); strings.Contains(text, "authorization_pending") || strings.Contains(text, "slow_down") {
			return Credentials{}, ErrAuthPending
		}
		return Credentials{}, apiError(http.MethodPost, c.workOSTokenURL, status, raw)
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
	endpoint := c.apiBase + "/auth/register"
	status, raw, err := c.postJSON(ctx, endpoint, map[string]string{
		"accessToken":  workosAccess,
		"refreshToken": strings.TrimSpace(workosRefresh),
	})
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		return Credentials{}, apiError(http.MethodPost, endpoint, status, raw)
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
	endpoint := c.apiBase + "/auth/refresh"
	status, raw, err := c.postJSON(ctx, endpoint, map[string]string{
		"refreshToken": refreshToken,
		"grantType":    "refresh_token",
	})
	if err != nil {
		return Credentials{}, err
	}
	if status != http.StatusOK {
		err := apiError(http.MethodPost, endpoint, status, raw)
		refused := status == http.StatusUnauthorized || status == http.StatusForbidden
		if refused || status == http.StatusBadRequest {
			// The Cline refresh token is the WorkOS refresh token that
			// /auth/register wrapped, so WorkOS can still renew it when the
			// Cline endpoint refuses. That saves a browser login; only when
			// WorkOS refuses too is the grant really gone.
			if renewed, ok := c.refreshViaWorkOS(ctx, refreshToken); ok {
				return renewed, nil
			}
		}
		if refused {
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

// refreshViaWorkOS renews the credential with the WorkOS refresh_token grant.
// It reports false on any failure: the caller keeps the Cline endpoint's own
// verdict, which is the one the operator is shown.
func (c *Client) refreshViaWorkOS(ctx context.Context, refreshToken string) (Credentials, bool) {
	status, raw, err := c.postForm(ctx, c.workOSTokenURL, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.workOSClientID},
		"refresh_token": {refreshToken},
	})
	if err != nil || status != http.StatusOK {
		return Credentials{}, false
	}
	var granted struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &granted); err != nil || strings.TrimSpace(granted.AccessToken) == "" {
		return Credentials{}, false
	}
	access := strings.TrimSpace(granted.AccessToken)
	rotated := util.FirstNonEmptyUntrimmed(strings.TrimSpace(granted.RefreshToken), refreshToken)
	// The renewed WorkOS grant is exchanged for a Cline pair exactly as at
	// login, so the account keeps the credential the Cline endpoints issue.
	// The bare WorkOS token is the fallback when that exchange fails.
	if registered, err := c.Register(ctx, access, rotated); err == nil {
		registered.RefreshToken = util.FirstNonEmptyUntrimmed(registered.RefreshToken, rotated)
		return registered, true
	}
	return Credentials{
		AccessToken:  access,
		RefreshToken: rotated,
		ExpiresAt:    jwtExpiry(access),
	}, true
}

// jwtExpiry reads the exp claim of a WorkOS access token. The signature is not
// checked: the value only schedules the next renewal, and the upstream stays
// the authority on validity. An unreadable token yields zero, which is tried
// as-is until the upstream refuses it.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	claims, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(claims, &payload); err != nil || payload.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(payload.Exp, 0).UTC()
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
