package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

type cliRotationKey struct {
	owner *store.Store
	id    int64
}

var cliOAuthPendingRotations sync.Map // cliRotationKey -> GrokCredentialPatch

// CLIOAuth handles the Build CLI OAuth token lifecycle: return the current
// access_token when unexpired, otherwise refresh via the refresh_token grant
// and persist the rotated pair back to Redis.

const (
	cliOAuthRefreshSkew  = 5 * time.Minute
	cliOAuthMaxBodyBytes = 1 << 20
)

// CLIOAuth refreshes Grok Build OAuth tokens.
type CLIOAuth struct {
	cfg        *config.Config
	httpClient *http.Client
	store      *store.Store
}

// NewCLIOAuth builds an OAuth helper for the CLI upstream.
func NewCLIOAuth(cfg *config.Config, httpClient *http.Client) *CLIOAuth {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &CLIOAuth{cfg: cfg, httpClient: httpClient}
}

// SetAccountStore enables durable persistence of rotated OAuth tokens.
func (o *CLIOAuth) SetAccountStore(s *store.Store) {
	if o == nil {
		return
	}
	o.store = s
}

// AccessToken returns a valid access token for the account. It refreshes and
// persists when the stored token is missing or within skew of expiry.
func (o *CLIOAuth) AccessToken(ctx context.Context, acc *store.Account) (string, error) {
	return o.accessToken(ctx, acc, false)
}

// ForceRefresh holds the same rotation lock but bypasses the validity shortcut.
func (o *CLIOAuth) ForceRefresh(ctx context.Context, acc *store.Account) (string, error) {
	return o.accessToken(ctx, acc, true)
}

func (o *CLIOAuth) accessToken(ctx context.Context, acc *store.Account, force bool) (string, error) {
	if acc == nil {
		return "", fmt.Errorf("empty cli oauth account")
	}
	var owner any = acc
	id := acc.ID
	if o.store != nil {
		owner = o.store
	}
	if id == 0 {
		id = -1
	}
	release, err := refreshqueue.AcquireCredential(ctx, "grok", owner, id)
	if err != nil {
		return "", err
	}
	defer release()
	rejected := acc.OAuthAccessToken
	key := cliRotationKey{o.store, acc.ID}
	if patch, ok := cliOAuthPendingRotations.Load(key); ok {
		pending := patch.(store.GrokCredentialPatch)
		if err := o.persistRotation(ctx, acc.ID, pending); err != nil {
			latest, readErr := refreshqueue.LatestAccount(ctx, o.store, acc.ID)
			// A later reauthorization supersedes a pending rotation. Never block
			// that credential or overwrite it with a pre-reauthorization grant.
			if readErr != nil || latest == nil || latest.OAuthRefreshToken == pending.ExpectedRefreshToken || latest.OAuthRefreshToken == pending.RefreshToken {
				return "", err
			}
		}
		cliOAuthPendingRotations.Delete(key)
	}
	if o != nil && o.store != nil && acc.ID != 0 {
		latest, err := refreshqueue.LatestAccount(ctx, o.store, acc.ID)
		if err != nil {
			return "", err
		}
		if latest != nil {
			acc.OAuthAccessToken = latest.OAuthAccessToken
			acc.OAuthRefreshToken = latest.OAuthRefreshToken
			acc.OAuthExpiresAt = latest.OAuthExpiresAt
			acc.GrokRefresh = latest.GrokRefresh
		}
	}
	accessToken := strings.TrimSpace(acc.OAuthAccessToken)
	refreshToken := strings.TrimSpace(acc.OAuthRefreshToken)
	expiresAt := acc.OAuthExpiresAt

	if (!force || accessToken != rejected) && accessToken != "" && (expiresAt.IsZero() || time.Until(expiresAt) > cliOAuthRefreshSkew) {
		return accessToken, nil
	}
	// While the scheduler backs off a failed refresh, a request keeps using
	// the token that is still valid instead of retrying the refresh itself.
	now := time.Now()
	if acc.GrokRefresh.InBackoff(now) {
		if !force && cliAccessTokenAlive(acc, now) {
			return accessToken, nil
		}
		// No usable token and the refresh is backing off: every request would
		// retry a refresh the scheduler just saw fail. Fail fast instead; the
		// scheduler retries when the backoff ends.
		return "", &cliOAuthError{status: http.StatusServiceUnavailable, message: "grok cli oauth refresh is backing off after " + acc.GrokRefresh.ErrorCode,
			code: "refresh_backoff", class: cliOAuthTransient}
	}
	if refreshToken == "" {
		return "", &cliOAuthError{status: http.StatusUnauthorized, message: "grok cli oauth refresh token is missing", code: "missing_refresh_token", class: cliOAuthPermanent}
	}
	token, err := o.refreshAndPersist(ctx, acc, refreshToken)
	// acc still holding the old token means the grant itself failed, not the
	// persistence of a rotation that already succeeded.
	if err != nil && !force && accessToken != "" && strings.TrimSpace(acc.OAuthAccessToken) == accessToken && cliAccessTokenAlive(acc, now) {
		// An early renewal failed but the current token has not expired: serve
		// it. The scheduler records the failure and decides about a re-login;
		// failing the request here would retire a credential that still works.
		slog.Debug("grok cli oauth: early refresh failed; using the unexpired token", "account_id", acc.ID, "error", err)
		return accessToken, nil
	}
	return token, err
}

func (o *CLIOAuth) refreshAndPersist(ctx context.Context, acc *store.Account, refreshToken string) (string, error) {
	accessToken, newRefresh, identityToken, expiresAt, err := o.refresh(ctx, refreshToken)
	if err != nil {
		return "", err
	}
	// Persist the rotated tokens back to the account (refresh tokens can rotate).
	acc.OAuthAccessToken = accessToken
	acc.OAuthExpiresAt = expiresAt
	if newRefresh != "" && newRefresh != refreshToken {
		acc.OAuthRefreshToken = newRefresh
	}
	ApplyCLIOAuthIdentity(acc)
	ApplyCLIOAuthIdentityToken(acc, identityToken)
	if o != nil && o.store != nil && acc.ID != 0 {
		patch := store.GrokCredentialPatch{ExpectedRefreshToken: refreshToken, AccessToken: acc.OAuthAccessToken,
			RefreshToken: acc.OAuthRefreshToken, ExpiresAt: acc.OAuthExpiresAt, UserID: acc.UserID, Email: acc.Email, Name: acc.Name, TeamID: acc.TeamID}
		key := cliRotationKey{o.store, acc.ID}
		cliOAuthPendingRotations.Store(key, patch)
		if updateErr := o.persistRotation(ctx, acc.ID, patch); updateErr != nil {
			slog.Warn("grok cli oauth: failed to persist rotated tokens", "account_id", acc.ID, "error", updateErr)
			return "", updateErr
		}
		cliOAuthPendingRotations.Delete(key)
	}
	return accessToken, nil
}

func (o *CLIOAuth) persistRotation(ctx context.Context, id int64, patch store.GrokCredentialPatch) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return o.store.UpdateGrokCredentials(writeCtx, id, patch)
}

// refresh performs the OAuth refresh_token grant against auth.x.ai.
func (o *CLIOAuth) refresh(ctx context.Context, refreshToken string) (accessToken, newRefresh, identityToken string, expiresAt time.Time, err error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("client_id", o.clientID())
	form.Set("refresh_token", refreshToken)

	body, status, err := postOAuthForm(ctx, o.httpClient, o.tokenURL(), form, cliOAuthMaxBodyBytes, nil, parseCLIOAuthErrorResponse)
	if err != nil {
		return "", "", "", time.Time{}, err
	}

	var value struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return "", "", "", time.Time{}, fmt.Errorf("grok cli oauth refresh parse: %w", err)
	}
	if strings.TrimSpace(value.AccessToken) == "" {
		return "", "", "", time.Time{}, &cliOAuthError{status: status, message: "grok cli oauth response missing access_token", code: "missing_access_token", class: cliOAuthTransient}
	}
	expiresIn := value.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	recordCLIOAuthRefresh()
	return strings.TrimSpace(value.AccessToken), strings.TrimSpace(value.RefreshToken), strings.TrimSpace(value.IDToken), time.Now().UTC().Add(time.Duration(expiresIn) * time.Second), nil
}

func (o *CLIOAuth) clientID() string {
	if o != nil && o.cfg != nil {
		return o.cfg.GrokCLIOAuthClientIDOrDefault()
	}
	return "b1a00492-073a-47ea-816f-4c329264a828"
}

func (o *CLIOAuth) tokenURL() string {
	if o != nil && o.cfg != nil {
		return o.cfg.GrokCLIOAuthTokenURLOrDefault()
	}
	return "https://auth.x.ai/oauth2/token"
}

// parseCLIOAuthErrorResponse maps an OAuth error body to a status-coded error.
func parseCLIOAuthErrorResponse(body []byte, status int) error {
	var payload struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	code := fmt.Sprintf("oauth_http_%d", status)
	_ = json.Unmarshal(body, &payload)
	if payload.Error != "" {
		code = payload.Error
	}
	// OAuth diagnostics can echo bearer tokens, cookies, or JWTs. Keep the
	// stable error code only; callers may log this error.
	class := classifyCLIOAuthError(code, status)
	if class == cliOAuthPermanent {
		// A refused grant surfaces as 401 so the account is cooled down.
		return &cliOAuthError{status: http.StatusUnauthorized, message: "grok cli oauth refresh denied: " + code, code: code, class: class}
	}
	return &cliOAuthError{status: status, message: "grok cli oauth refresh failed (" + code + ")", code: code, class: class}
}

// The classes of a failed refresh. Only cliOAuthPermanent is a verdict on the
// credential; the others leave the account in rotation with a backoff.
const (
	// cliOAuthPermanent: the refresh grant itself was refused (revoked,
	// expired, reused). Only a new login restores the account.
	cliOAuthPermanent = "permanent"
	// cliOAuthConfiguration: the request was refused for the client, not the
	// account (client_id, grant type). Every account would fail the same way,
	// so none is retired for it.
	cliOAuthConfiguration = "configuration"
	// cliOAuthUnclassified: a 4xx with no recognised code. Repeated, and with
	// no live access token left, it ends in a re-login.
	cliOAuthUnclassified = "unclassified"
	// cliOAuthTransient: throttling, server errors and transport failures.
	cliOAuthTransient = "transient"
)

// classifyCLIOAuthError sorts an OAuth refresh error. Only explicit
// credential-specific codes are permanent: treating every 400/401 as terminal
// let one misconfigured client id, or one unexplained refusal, send a whole
// pool to re-login.
func classifyCLIOAuthError(code string, status int) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "invalid_grant", "refresh_denied", "expired_token", "invalid_refresh_token",
		"refresh_token_invalid", "refresh_token_expired", "refresh_token_revoked",
		"refresh_token_reused", "refresh_token_reuse", "token_reused", "token_reuse_detected", "revoked_token":
		return cliOAuthPermanent
	case "invalid_client", "unauthorized_client", "unsupported_grant_type", "invalid_scope":
		return cliOAuthConfiguration
	}
	if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
		return cliOAuthUnclassified
	}
	return cliOAuthTransient
}

// cliOAuthFailure returns the class and code of a refresh failure. Anything
// that is not a typed OAuth refusal (a timeout, a reset connection) is transient.
func cliOAuthFailure(err error) (class, code string) {
	if oauthErr, ok := errors.AsType[*cliOAuthError](err); ok && oauthErr.class != "" {
		return oauthErr.class, oauthErr.code
	}
	return cliOAuthTransient, "transport_error"
}

// IsCLIPermanentOAuthError reports whether the refresh grant itself was
// refused, as opposed to a configuration, unexplained or transient failure.
func IsCLIPermanentOAuthError(err error) bool {
	class, _ := cliOAuthFailure(err)
	return class == cliOAuthPermanent
}
