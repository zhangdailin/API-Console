package warp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const authRequestTimeout = 5 * time.Second

// Only allowlisted verdicts escape the exchange. Never retain a raw response,
// request URL, token, or transport error in an authentication error.
type authFailure struct {
	reason string
	delay  time.Duration
}

func (e *authFailure) terminal() bool {
	switch e.reason {
	case "TOKEN_EXPIRED", "INVALID_REFRESH_TOKEN", "MISSING_REFRESH_TOKEN", "USER_DISABLED", "USER_NOT_FOUND":
		return true
	}
	return false
}

func (e *authFailure) Error() string {
	switch e.reason {
	case "TOKEN_EXPIRED", "INVALID_REFRESH_TOKEN", "MISSING_REFRESH_TOKEN":
		return "401: Warp refresh token rejected (" + e.reason + "); sign in again"
	case "USER_DISABLED", "USER_NOT_FOUND":
		return "401: Warp account disabled or deleted (" + e.reason + "); official login required"
	}
	return "HTTP 503: Warp authentication temporarily unavailable (" + safeAuthReason(e.reason) + ")"
}

func (e *authFailure) RetryAfter() time.Duration { return e.delay }

func safeAuthReason(reason string) string {
	switch reason {
	case "transport", "invalid_response", "firebase_error", "http_status", "proxy_unavailable":
		return reason
	}
	return "invalid_response"
}

func transientAuthFailure(reason string, delay time.Duration) *authFailure {
	return &authFailure{reason: safeAuthReason(reason), delay: delay}
}

func firebaseAuthFailure(message string) *authFailure {
	failure := &authFailure{reason: message}
	if failure.terminal() {
		return failure
	}
	return transientAuthFailure("firebase_error", 0)
}

func authRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		return time.Duration(min(max(seconds, 1), 300)) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return min(max(at.Sub(now), 0), 5*time.Minute)
	}
	return 0
}

// Custom endpoints never receive production proxy fallback, nor can their
// synthetic or third-party credentials be forwarded to the official service.
func (c *Client) officialAuthEndpoints() bool {
	return c.base == DefaultBaseURL && c.firebaseBase == "https://identitytoolkit.googleapis.com" && c.tokenBase == "https://securetoken.googleapis.com"
}

func (c *Client) exchangeAuthTokens(ctx context.Context, direct, proxyPath string, fields ...string) (Credentials, error) {
	if err := ctx.Err(); err != nil {
		return Credentials{}, err
	}
	form := url.Values{}
	for i := 0; i+1 < len(fields); i += 2 {
		form.Set(fields[i], fields[i+1])
	}
	body := []byte(form.Encode())
	grant, err := c.fetchAuthTokens(ctx, direct, body, false)
	if err == nil || ctx.Err() != nil {
		return grant, err
	}
	var failure *authFailure
	if !errors.As(err, &failure) || failure.terminal() || !c.officialAuthEndpoints() {
		return Credentials{}, err
	}
	proxy := DefaultBaseURL + proxyPath + "?" + url.Values{"key": {c.firebaseKey}}.Encode()
	return c.fetchAuthTokens(ctx, proxy, body, true)
}

func (c *Client) fetchAuthTokens(ctx context.Context, endpoint string, body []byte, proxy bool) (Credentials, error) {
	requestCtx, cancel := context.WithTimeout(ctx, authRequestTimeout)
	defer cancel()
	resp, err := c.request(requestCtx, endpoint, "application/x-www-form-urlencoded", body, "")
	if err != nil {
		if ctx.Err() != nil {
			return Credentials{}, ctx.Err()
		}
		return Credentials{}, transientAuthFailure("transport", 0)
	}
	defer resp.Body.Close()
	delay := authRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if proxy && (resp.StatusCode == 429 || resp.StatusCode == 408 || resp.StatusCode >= 500) {
		return Credentials{}, transientAuthFailure("proxy_unavailable", delay)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if ctx.Err() != nil {
		return Credentials{}, ctx.Err()
	}
	if err != nil || len(raw) > 2<<20 {
		return Credentials{}, transientAuthFailure("invalid_response", delay)
	}
	var answer struct {
		IDToken      string          `json:"id_token"`
		IDTokenCamel string          `json:"idToken"`
		Refresh      string          `json:"refresh_token"`
		RefreshCamel string          `json:"refreshToken"`
		Expires      string          `json:"expires_in"`
		ExpiresCamel string          `json:"expiresIn"`
		Error        json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &answer) != nil {
		return Credentials{}, transientAuthFailure("invalid_response", delay)
	}
	if len(answer.Error) != 0 {
		var verdict struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(answer.Error, &verdict) == nil {
			failure := firebaseAuthFailure(verdict.Message)
			failure.delay = delay
			return Credentials{}, failure
		}
		return Credentials{}, transientAuthFailure("firebase_error", delay)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Credentials{}, transientAuthFailure("http_status", delay)
	}
	if (answer.IDToken != "" && answer.IDTokenCamel != "" && answer.IDToken != answer.IDTokenCamel) ||
		(answer.Refresh != "" && answer.RefreshCamel != "" && answer.Refresh != answer.RefreshCamel) ||
		(answer.Expires != "" && answer.ExpiresCamel != "" && answer.Expires != answer.ExpiresCamel) {
		return Credentials{}, transientAuthFailure("invalid_response", delay)
	}
	if answer.IDToken == "" {
		answer.IDToken = answer.IDTokenCamel
	}
	if answer.Refresh == "" {
		answer.Refresh = answer.RefreshCamel
	}
	if answer.Expires == "" {
		answer.Expires = answer.ExpiresCamel
	}
	expires, err := expiry(answer.Expires)
	if err != nil || answer.IDToken == "" {
		return Credentials{}, transientAuthFailure("invalid_response", delay)
	}
	return Credentials{AccessToken: answer.IDToken, RefreshToken: answer.Refresh, ExpiresAt: expires}, nil
}
