package qoder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/util"
)

// The Qoder login is a custom device authorization grant, not a standard OAuth
// authorization code flow. There is no redirect URI and no local callback:
//
//	1. the server mints a PKCE pair plus a nonce and machine id,
//	2. the operator opens the official page and signs in there,
//	3. the server polls the device token endpoint until the page completes.
//
// The bridge never sees the password, and the polling response is the only
// place a credential enters the pool.

// authRequestTimeout bounds a single control-plane request.
const authRequestTimeout = 30 * time.Second

// LoginTTL bounds one authorization transaction. The CLI waits 300s; the admin
// console keeps a longer window because the operator may still have to sign in
// to the Qoder website first.
const LoginTTL = 15 * time.Minute

// LoginTransaction is one in-flight device authorization.
type LoginTransaction struct {
	// VerifyURL is the official page the operator must open.
	VerifyURL string
	// Nonce and Verifier are the transaction's private halves. They never leave
	// the server: only the challenge is exposed, in the verify URL.
	Nonce    string
	Verifier string
	// MachineID is the device identity the credential will be bound to.
	MachineID string
	// ExpiresAt bounds polling.
	ExpiresAt time.Time
}

// StartLogin mints one device authorization transaction.
func (c *Client) StartLogin(ctx context.Context) (*LoginTransaction, error) {
	if c == nil {
		return nil, fmt.Errorf("qoder client is nil")
	}
	verifier, challenge, err := pkcePair(c.entropy)
	if err != nil {
		return nil, err
	}
	nonce, err := newUUID(c.entropy)
	if err != nil {
		return nil, err
	}
	machineID := strings.TrimSpace(c.machineID)
	if machineID == "" {
		// An account-less client is a login client: it mints the device identity
		// that the credential will be persisted with.
		if machineID, err = newUUID(c.entropy); err != nil {
			return nil, err
		}
	}

	query := url.Values{}
	query.Set("challenge", challenge)
	query.Set("challenge_method", "S256")
	query.Set("nonce", nonce)
	query.Set("machine_id", machineID)
	query.Set("client_id", c.clientID)
	if version := c.clientVersion; version != "" {
		query.Set("client_version", version)
	}
	verifyURL := c.endpoints.oauth + "/device/selectAccounts?" + query.Encode()

	// Only an allowed authorization host may ever be handed to a browser: this
	// is the one place a login could be redirected to a third party.
	if host := util.HostOf(verifyURL); !c.allowedLoginHost(host) {
		return nil, fmt.Errorf("%w: authorization host %q is not an allowed Qoder host", ErrAuthRejected, host)
	}

	// Confirm the authorization page answers before handing the URL to the
	// operator. A blocked egress path otherwise looks like "the login button
	// does nothing".
	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	if err := c.probeHost(reqCtx, c.endpoints.oauth); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}

	return &LoginTransaction{
		VerifyURL: verifyURL,
		Nonce:     nonce,
		Verifier:  verifier,
		MachineID: machineID,
		ExpiresAt: time.Now().Add(LoginTTL),
	}, nil
}

// PollLogin exchanges one authorization attempt for a credential pair.
// ErrAuthPending means the browser step has not completed yet — the device
// token endpoint answers HTTP 404 until it has.
func (c *Client) PollLogin(ctx context.Context, tx *LoginTransaction) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("qoder client is nil")
	}
	if tx == nil {
		return Credentials{}, fmt.Errorf("qoder login transaction is missing")
	}
	query := url.Values{}
	query.Set("nonce", tx.Nonce)
	query.Set("verifier", tx.Verifier)
	query.Set("challenge_method", "S256")
	endpoint := c.endpoints.openAPI + "/api/v1/deviceToken/poll?" + query.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Credentials{}, fmt.Errorf("build device token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent(c.clientVersion))

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// The CLI treats 404 as "the browser step is not done yet"; it is the
		// documented pending signal, not a missing endpoint.
		return Credentials{}, ErrAuthPending
	case resp.StatusCode != http.StatusOK:
		return Credentials{}, fmt.Errorf("%w: %v", ErrAuthRejected, apiError(http.MethodGet, endpoint, resp.StatusCode, raw))
	}

	var token DeviceToken
	if err := json.Unmarshal(raw, &token); err != nil {
		return Credentials{}, fmt.Errorf("%w: undecodable device token response", ErrAuthRejected)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return Credentials{}, fmt.Errorf("%w: device token response carried no token", ErrAuthRejected)
	}
	token.applyExpiries(time.Now())

	return Credentials{
		AccessToken:      strings.TrimSpace(token.AccessToken),
		RefreshToken:     strings.TrimSpace(token.RefreshToken),
		AccessExpiresAt:  token.AccessExpireAt,
		RefreshExpiresAt: token.RefreshExpireAt,
		UID:              strings.TrimSpace(token.UserID),
		Name:             strings.TrimSpace(token.UserName),
	}, nil
}

// Refresh exchanges the durable refresh token for a new access token. The
// upstream rotates the refresh token, so the returned pair must be persisted or
// the account dies at the next refresh.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Credentials, error) {
	if c == nil {
		return Credentials{}, fmt.Errorf("qoder client is nil")
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return Credentials{}, ErrCredentialMissing
	}
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return Credentials{}, fmt.Errorf("marshal refresh request: %w", err)
	}
	endpoint := c.endpoints.openAPI + "/api/v1/deviceToken/refresh"

	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Credentials{}, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent(c.clientVersion))

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Credentials{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		err := apiError(http.MethodPost, endpoint, resp.StatusCode, raw)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || IsSessionDead(string(raw)) {
			// The refresh grant itself was refused, or the upstream said the
			// session is gone under another status: no local retry can recover
			// it, and the operator has to authorize again.
			return Credentials{}, fmt.Errorf("%w: %v", ErrReLoginRequired, err)
		}
		return Credentials{}, err
	}

	var payload struct {
		DeviceToken      string `json:"device_token"`
		Token            string `json:"token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresAt        string `json:"expires_at"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresAt string `json:"refresh_token_expires_at"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Credentials{}, fmt.Errorf("decode refresh response: %w", err)
	}
	// The refresh endpoint names the access token `device_token`, while the poll
	// endpoint names it `token`; both spellings are accepted.
	access := util.FirstNonEmptyUntrimmed(payload.DeviceToken, payload.Token)
	if access == "" {
		return Credentials{}, fmt.Errorf("%w: refresh response carried no token", ErrReLoginRequired)
	}
	now := time.Now()
	return Credentials{
		AccessToken:      strings.TrimSpace(access),
		RefreshToken:     strings.TrimSpace(payload.RefreshToken),
		AccessExpiresAt:  parseExpiry(payload.ExpiresAt, payload.ExpiresIn, now),
		RefreshExpiresAt: parseExpiry(payload.RefreshExpiresAt, payload.RefreshExpiresIn, now),
	}, nil
}

// FetchProfile reads the signed-in profile. It is optional metadata: callers
// must tolerate its failure, because a userinfo outage must not invalidate an
// otherwise working credential.
func (c *Client) FetchProfile(ctx context.Context, accessToken string) (Profile, error) {
	if c == nil {
		return Profile{}, fmt.Errorf("qoder client is nil")
	}
	endpoint := c.endpoints.openAPI + "/api/v1/userinfo"
	reqCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("User-Agent", userAgent(c.clientVersion))

	resp, raw, err := httpclient.DoReadBody(c.control, req, 64<<10)
	if err != nil {
		return Profile{}, fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return Profile{}, apiError(http.MethodGet, endpoint, resp.StatusCode, raw)
	}
	var profile Profile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return Profile{}, fmt.Errorf("decode userinfo response: %w", err)
	}
	return profile, nil
}

// probeHost performs a cheap reachability check against one control host so a
// blocked egress path is reported at login time instead of as a per-request
// timeout later.
func (c *Client) probeHost(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/", nil)
	if err != nil {
		return err
	}
	resp, err := c.control.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	return nil
}

// ProbeReachability reports whether this process can reach the Qoder control
// plane. It carries no credentials and is meant for startup diagnostics.
func (c *Client) ProbeReachability(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("qoder client is nil")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.probeHost(reqCtx, c.endpoints.openAPI); err != nil {
		return fmt.Errorf("%w: %v", ErrAuthUnavailable, err)
	}
	return c.probeHost(reqCtx, c.endpoints.oauth)
}

// allowedLoginHost reports whether an authorization URL may be handed to a
// browser.
//
// The allowlist is the Qoder hosts plus the deployment's own configured OAuth
// endpoint. Honouring the configured endpoint is deliberate: a self-hosted or
// regional deployment legitimately points the flow somewhere else, and refusing
// it would make the setting unusable. Everything else is refused, including a
// host that merely looks similar.
func (c *Client) allowedLoginHost(host string) bool {
	return util.AllowedLoginHost(host, strings.ToLower(util.HostOf(c.endpoints.oauth)), loginHosts)
}
