package grok

import (
	"context"
	"errors"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

// ErrDeviceAuthorizationPending is the sentinel a pending or slow-down answer
// matches with errors.Is.
var ErrDeviceAuthorizationPending = errors.New("grok device authorization pending")

// Is lets the login flow recognise a pending answer by sentinel.
func (e *DeviceAuthorizationPendingError) Is(target error) bool {
	return target == ErrDeviceAuthorizationPending
}

// deviceGrant is what a completed device authorization produced.
type deviceGrant struct {
	accessToken   string
	refreshToken  string
	identityToken string
	expiresAt     time.Time
}

// loginPort is the official xAI Grok Build CLI device-authorization flow. It
// accepts no files, browser cookies, passwords, or user-supplied tokens; device
// codes remain server-side only.
func loginPort() *provider.Login {
	return &provider.Login{
		ErrPending:      ErrDeviceAuthorizationPending,
		NeedsDeviceCode: true,
		Start: func(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
			authenticator := NewDeviceAuthenticator(cfg)
			details, err := authenticator.Start(ctx)
			if err != nil {
				return provider.LoginChallenge{}, err
			}
			return provider.LoginChallenge{
				DeviceCode: details.DeviceCode,
				UserCode:   details.UserCode,
				VerifyURI:  details.VerificationURI,
				VerifyFull: details.VerificationURIComplete,
				ExpiresAt:  time.Now().Add(time.Duration(details.ExpiresIn) * time.Second),
				Interval:   time.Duration(details.Interval) * time.Second,
				// One authenticator serves the whole transaction, so the start
				// and every poll use the same endpoints and connection pool.
				Private: authenticator,
			}, nil
		},
		Exchange: func(ctx context.Context, cfg *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
			authenticator, ok := challenge.Private.(*DeviceAuthenticator)
			if !ok || authenticator == nil {
				authenticator = NewDeviceAuthenticator(cfg)
			}
			accessToken, refreshToken, identityToken, expiresAt, err := authenticator.Exchange(ctx, challenge.DeviceCode)
			if err != nil {
				return nil, err
			}
			return deviceGrant{accessToken: accessToken, refreshToken: refreshToken, identityToken: identityToken, expiresAt: expiresAt}, nil
		},
		Build: func(_ context.Context, _ *config.Config, _ string, _ provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
			tokens, ok := grant.(deviceGrant)
			if !ok {
				return nil, errors.New("grok login produced an unexpected grant")
			}
			acc := &store.Account{
				Name:              deviceLoginAccountName,
				AccountType:       "grok",
				CredentialType:    "oauth",
				OAuthAccessToken:  tokens.accessToken,
				OAuthRefreshToken: tokens.refreshToken,
				OAuthExpiresAt:    tokens.expiresAt,
				Weight:            1,
				Enabled:           true,
			}
			ApplyCLIOAuthIdentity(acc)
			ApplyCLIOAuthIdentityToken(acc, tokens.identityToken)
			normalizeTokenInput(acc)
			return acc, nil
		},
		SlowDown: func(err error) bool {
			slowDown, _ := IsDeviceAuthorizationPending(err)
			return slowDown
		},
		Merge: mergeDeviceLogin,
	}
}

// deviceLoginAccountName is the placeholder name of an account whose identity
// token carried no address.
const deviceLoginAccountName = "grok-device-login"

// mergeDeviceLogin writes a fresh device grant onto the row the same xAI user
// already has.
//
// A fresh grant may rotate the durable refresh token. The matching identity is
// updated in place and a legacy generic row is enriched with the email obtained
// from id_token, while the operator settings and the accumulated runtime state
// already held on the row are retained.
func mergeDeviceLogin(existing, fresh *store.Account) {
	existing.OAuthAccessToken = fresh.OAuthAccessToken
	existing.OAuthRefreshToken = fresh.OAuthRefreshToken
	existing.OAuthExpiresAt = fresh.OAuthExpiresAt
	existing.CredentialType = "oauth"
	existing.GrokProvider = ProviderBuild
	existing.AgentMode = fresh.AgentMode
	if fresh.UserID != "" {
		existing.UserID = fresh.UserID
	}
	if fresh.Email != "" {
		existing.Email = fresh.Email
		if existing.Name == "" || strings.EqualFold(existing.Name, deviceLoginAccountName) {
			existing.Name = fresh.Email
		}
	}
	if fresh.TeamID != "" {
		existing.TeamID = fresh.TeamID
	}
	existing.StatusCode = ""
	existing.StatusMessage = ""
	existing.LastAttempt = time.Time{}
	existing.ClearVerifiedAt = true
}
