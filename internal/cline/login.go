package cline

import (
	"context"
	"errors"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

const (
	// loginInterval is the poll cadence. WorkOS states its own; this is the
	// ceiling when it does not, and it is deliberately slower than the CLI's
	// single-second poll so a stalled transaction cannot hammer the endpoint
	// for fifteen minutes.
	loginInterval = 2 * time.Second
	// loginTTL bounds one transaction on this side.
	loginTTL = 15 * time.Minute
)

// loginPort is the official Cline device authorization flow.
//
// Only the user code and the official page URL ever reach the browser. The
// device code — the half that can be exchanged for a credential — stays
// server-side, so a compromised browser session cannot mint a credential from a
// half-finished transaction.
func loginPort() *provider.Login {
	return &provider.Login{
		UnreachableDetail: "the server cannot reach the Cline authorization endpoint",
		ErrPending:        ErrAuthPending,
		ErrUnavailable:    ErrAuthUnavailable,
		ErrRejected:       ErrAuthRejected,
		NeedsDeviceCode:   true,
		Start: func(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
			client := NewFromAccount(nil, cfg)
			defer client.Close()
			transaction, err := client.StartLogin(ctx)
			if err != nil {
				return provider.LoginChallenge{}, err
			}
			expiresAt := transaction.ExpiresAt
			if expiresAt.IsZero() || expiresAt.After(time.Now().Add(loginTTL)) {
				expiresAt = time.Now().Add(loginTTL)
			}
			return provider.LoginChallenge{
				DeviceCode: transaction.DeviceCode,
				UserCode:   transaction.UserCode,
				VerifyURI:  transaction.VerifyURL,
				VerifyFull: transaction.VerifyFull,
				ExpiresAt:  expiresAt,
				Interval:   loginInterval,
			}, nil
		},
		Exchange: func(ctx context.Context, cfg *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
			client := NewFromAccount(nil, cfg)
			defer client.Close()
			return client.PollLogin(ctx, &LoginTransaction{DeviceCode: challenge.DeviceCode, ExpiresAt: challenge.ExpiresAt})
		},
		Build: func(ctx context.Context, cfg *config.Config, loginID string, _ provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
			creds, ok := grant.(Credentials)
			if !ok {
				return nil, errors.New("cline login produced an unexpected grant")
			}
			return BuildLoginAccount(ctx, cfg, loginID, creds)
		},
		MarkReplace: func(acc *store.Account) { acc.ReplaceClineCredentials = true },
	}
}

// BuildLoginAccount turns a completed login into an account
// record.
//
// What makes a login succeed is the exchange: the WorkOS grant was traded for a
// Cline refresh token, and that token is what can renew the account. The model
// feed read is enrichment, not a credential check, so a feed that does not
// answer must not cost the operator a valid credential.
func BuildLoginAccount(ctx context.Context, cfg *config.Config, loginID string, creds Credentials) (*store.Account, error) {
	normalized := normalizeLoginResult(creds)
	acc := &store.Account{
		Name:              "cline-login",
		AccountType:       "cline",
		ClineAccessToken:  normalized.AccessToken,
		ClineRefreshToken: normalized.RefreshToken,
		ClineExpiresAt:    normalized.ExpiresAt,
		ClineEmail:        normalized.Email,
		Weight:            1,
		Enabled:           true,
	}
	if normalized.Email != "" {
		acc.Email = normalized.Email
		acc.Name = normalized.Email
	}

	client := NewFromAccount(acc, cfg)
	defer client.Close()

	observeAccount(ctx, client, acc, "login_id", loginID,
		"Cline upstream catalog read failed at login; leaving the snapshot empty",
		"Cline plan read failed at login; leaving the tier unset")

	// Without a refresh token the account cannot survive its first token expiry,
	// and the login would look successful until it silently dies.
	if strings.TrimSpace(acc.ClineRefreshToken) == "" {
		return nil, errors.New("cline login returned no refresh token")
	}
	return acc, nil
}

// normalizeLoginResult trims the credential a completed login produced.
func normalizeLoginResult(creds Credentials) Credentials {
	accessToken, refreshToken, expiresAt, email := creds.Fields()
	return Credentials{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresAt:    expiresAt,
		Email:        email,
	}
}
