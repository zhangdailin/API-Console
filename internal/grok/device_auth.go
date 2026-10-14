package grok

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"orchids-api/internal/httpclient"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
)

const (
	grokDeviceAuthorizationScope = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"
	grokDeviceOAuthMaxBodyBytes  = 1 << 20
)

// DeviceAuthorization is a short-lived official xAI device login. DeviceCode
// is intentionally never serialized or returned to the browser.
type DeviceAuthorization struct {
	DeviceCode              string `json:"-"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// DeviceAuthorizationPendingError tells the caller to poll again. SlowDown
// asks it to increase the interval, as required by RFC 8628.
type DeviceAuthorizationPendingError struct{ SlowDown bool }

func (e *DeviceAuthorizationPendingError) Error() string {
	if e != nil && e.SlowDown {
		return "grok device authorization slow down"
	}
	return "grok device authorization pending"
}

func IsDeviceAuthorizationPending(err error) (slowDown bool, ok bool) {
	var pending *DeviceAuthorizationPendingError
	if !errors.As(err, &pending) {
		return false, false
	}
	return pending.SlowDown, true
}

// DeviceAuthenticator implements xAI's official public-client Device
// Authorization flow. It does not use browser cookies, files, or passwords.
type DeviceAuthenticator struct {
	cfg        *config.Config
	httpClient *http.Client
	deviceURL  string
	tokenURL   string
}

func NewDeviceAuthenticator(cfg *config.Config) *DeviceAuthenticator {
	return &DeviceAuthenticator{
		cfg:        cfg,
		httpClient: newHTTPClient(cfg, 20*time.Second, httpclient.ProxyFuncFromConfig(cfg)),
		deviceURL:  cfg.GrokCLIOAuthDeviceURLOrDefault(),
		tokenURL:   cfg.GrokCLIOAuthTokenURLOrDefault(),
	}
}

func (a *DeviceAuthenticator) Start(ctx context.Context) (*DeviceAuthorization, error) {
	if a == nil || a.httpClient == nil {
		return nil, fmt.Errorf("grok device authenticator is not configured")
	}
	form := url.Values{
		"client_id": {a.clientID()},
		"scope":     {grokDeviceAuthorizationScope},
		"referrer":  {"grok-build"},
	}
	var upstream struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := a.postForm(ctx, a.deviceURL, form, &upstream); err != nil {
		return nil, fmt.Errorf("request grok device code: %w", err)
	}
	value := DeviceAuthorization{
		DeviceCode:              upstream.DeviceCode,
		UserCode:                upstream.UserCode,
		VerificationURI:         upstream.VerificationURI,
		VerificationURIComplete: upstream.VerificationURIComplete,
		ExpiresIn:               upstream.ExpiresIn,
		Interval:                upstream.Interval,
	}
	value.DeviceCode = strings.TrimSpace(value.DeviceCode)
	value.UserCode = strings.TrimSpace(value.UserCode)
	value.VerificationURI = strings.TrimSpace(value.VerificationURI)
	value.VerificationURIComplete = strings.TrimSpace(value.VerificationURIComplete)
	if value.DeviceCode == "" || value.UserCode == "" || value.VerificationURI == "" {
		return nil, fmt.Errorf("grok device code response is incomplete")
	}
	if value.ExpiresIn < 1 {
		value.ExpiresIn = 600
	}
	if value.Interval < 1 {
		value.Interval = 2
	}
	return &value, nil
}

// Exchange polls the official xAI token endpoint. identityToken is returned to
// the caller only long enough to extract non-secret claims such as email; it is
// never stored as account credential material.
func (a *DeviceAuthenticator) Exchange(ctx context.Context, deviceCode string) (accessToken, refreshToken, identityToken string, expiresAt time.Time, err error) {
	if a == nil || a.httpClient == nil {
		return "", "", "", time.Time{}, fmt.Errorf("grok device authenticator is not configured")
	}
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return "", "", "", time.Time{}, fmt.Errorf("missing grok device code")
	}
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"client_id":   {a.clientID()},
		"device_code": {deviceCode},
	}
	var value struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := a.postForm(ctx, a.tokenURL, form, &value); err != nil {
		return "", "", "", time.Time{}, err
	}
	accessToken = strings.TrimSpace(value.AccessToken)
	refreshToken = strings.TrimSpace(value.RefreshToken)
	if accessToken == "" || refreshToken == "" {
		return "", "", "", time.Time{}, fmt.Errorf("grok device token response is incomplete")
	}
	if value.ExpiresIn < 1 {
		value.ExpiresIn = 3600
	}
	return accessToken, refreshToken, strings.TrimSpace(value.IDToken), time.Now().UTC().Add(time.Duration(value.ExpiresIn) * time.Second), nil
}

func (a *DeviceAuthenticator) clientID() string {
	if a != nil && a.cfg != nil {
		return a.cfg.GrokCLIOAuthClientIDOrDefault()
	}
	return "b1a00492-073a-47ea-816f-4c329264a828"
}

func (a *DeviceAuthenticator) postForm(ctx context.Context, endpoint string, form url.Values, target any) error {
	body, _, err := postOAuthForm(ctx, a.httpClient, endpoint, form, grokDeviceOAuthMaxBodyBytes,
		func(req *http.Request) {
			if a.cfg != nil {
				req.Header.Set("x-grok-client-version", a.cfg.GrokCLIClientVersionOrDefault())
			}
			req.Header.Set("x-grok-client-surface", "ui")
		}, parseGrokDeviceOAuthError)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode grok device authorization response: %w", err)
	}
	return nil
}

// parseGrokDeviceOAuthError retains only the OAuth error code. OAuth servers
// can echo submitted device codes or token values in diagnostics, so raw
// response text must never be attached to a returned or logged error.
func parseGrokDeviceOAuthError(body []byte, status int) error {
	var payload struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &payload)
	code := strings.ToLower(strings.TrimSpace(payload.Error))
	switch code {
	case "authorization_pending":
		return &DeviceAuthorizationPendingError{}
	case "slow_down":
		return &DeviceAuthorizationPendingError{SlowDown: true}
	case "access_denied", "expired_token":
		return fmt.Errorf("grok device authorization denied: %s", code)
	case "":
		return fmt.Errorf("grok device authorization returned HTTP %d", status)
	default:
		return fmt.Errorf("grok device authorization failed: %s", code)
	}
}
