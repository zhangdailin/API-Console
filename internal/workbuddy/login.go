package workbuddy

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

const (
	// loginTTL is how long an authorization transaction stays alive. The
	// official desktop client waits 300s; the console keeps a longer window
	// because the operator may have to sign in to Keycloak first.
	loginTTL = 15 * time.Minute
	// loginInterval is the upstream poll cadence (the client uses 1s).
	loginInterval = 2 * time.Second
	// LoginClientVersion is appended to the login URL, as the desktop client
	// does.
	LoginClientVersion = "5.5.2"
)

// loginPort is the official WorkBuddy (www.workbuddy.ai) browser authorization
// flow. The server never sees the password: it only opens the official login
// page and polls for the resulting token pair.
func loginPort() *provider.Login {
	return &provider.Login{
		UnreachableDetail: "the server cannot reach www.workbuddy.ai",
		ErrPending:        ErrAuthPending,
		ErrUnavailable:    ErrAuthUnavailable,
		ErrRejected:       ErrAuthRejected,
		NeedsDeviceCode:   true,
		// The catalog read while building the account doubles as the credential
		// check, so a failed build is a failed verification.
		BuildVerifies: true,
		Start: func(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
			client := NewFromAccount(nil, cfg)
			defer client.Close()
			state, authURL, err := client.StartAuthLogin(ctx, LoginClientVersion)
			if err != nil {
				return provider.LoginChallenge{}, err
			}
			slog.Debug("WorkBuddy authorization started", "login_state_host", "www.workbuddy.ai")
			return provider.LoginChallenge{
				DeviceCode: state,
				VerifyFull: authURL,
				ExpiresAt:  time.Now().Add(loginTTL),
				Interval:   loginInterval,
			}, nil
		},
		Exchange: func(ctx context.Context, cfg *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
			client := NewFromAccount(nil, cfg)
			defer client.Close()
			return client.PollAuthLogin(ctx, challenge.DeviceCode)
		},
		Build: func(ctx context.Context, cfg *config.Config, _ string, challenge provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
			creds, ok := grant.(Credentials)
			if !ok {
				return nil, errors.New("workbuddy login produced an unexpected grant")
			}
			return BuildLoginAccount(ctx, cfg, challenge.DeviceCode, creds)
		},
		MarkReplace: func(acc *store.Account) { acc.ReplaceWorkBuddyCredentials = true },
	}
}

// BuildLoginAccount turns a completed login into a verified
// account record. The model catalog read doubles as the credential check: a
// token that cannot list the account catalog is never persisted.
func BuildLoginAccount(ctx context.Context, cfg *config.Config, state string, creds Credentials) (*store.Account, error) {
	acc := &store.Account{
		Name:                  "workbuddy-login",
		AccountType:           "workbuddy",
		WorkBuddyAccessToken:  strings.TrimSpace(creds.AccessToken),
		WorkBuddyRefreshToken: strings.TrimSpace(creds.RefreshToken),
		WorkBuddyUID:          strings.TrimSpace(creds.UID),
		Email:                 strings.TrimSpace(creds.Email),
		WorkBuddyExpiresAt:    creds.ExpiresAt,
		Weight:                1,
		Enabled:               true,
	}
	if acc.WorkBuddyExpiresAt.IsZero() && !creds.ExpiresAt.IsZero() {
		acc.WorkBuddyExpiresAt = creds.ExpiresAt
	}

	client := NewFromAccount(acc, cfg)
	defer client.Close()

	// The access-token JWT already proves the identity; the profile endpoint only
	// adds the display nickname, so its failure is not fatal.
	resolved := ResolveCredentials(acc)
	if uid, nickname, email, err := client.FetchAccountIdentity(ctx, acc.WorkBuddyAccessToken, state); err == nil {
		acc.Name = strings.TrimSpace(nickname)
		if acc.WorkBuddyUID == "" {
			acc.WorkBuddyUID = uid
		}
		if resolved.Email == "" && email != "" {
			acc.Email = email
		}
	}
	if acc.WorkBuddyUID == "" {
		acc.WorkBuddyUID = resolved.UID
	}
	if strings.TrimSpace(acc.Email) == "" {
		acc.Email = resolved.Email
	}
	if strings.TrimSpace(acc.Name) == "" {
		switch {
		case strings.TrimSpace(acc.Email) != "":
			acc.Name = acc.Email
		case len(acc.WorkBuddyUID) >= 8:
			acc.Name = "workbuddy-" + acc.WorkBuddyUID[:8]
		default:
			acc.Name = "workbuddy-login"
		}
	}

	models, err := client.FetchModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := CatalogSnapshot(models)
	if len(ids) == 0 {
		return nil, errors.New("workbuddy catalog was empty")
	}
	acc.WorkBuddyModelIDs = ids
	acc.WorkBuddyModelsSyncedAt = time.Now()

	if strings.TrimSpace(acc.WorkBuddyRefreshToken) == "" {
		// Without a refresh token the account cannot survive its first token
		// expiry; refresh immediately so the durable credential is captured.
		refreshed, refreshErr := client.RefreshCredentials(ctx)
		if refreshErr != nil || strings.TrimSpace(refreshed.RefreshToken) == "" {
			return nil, errors.New("workbuddy login returned no refresh token")
		}
		acc.WorkBuddyAccessToken = refreshed.AccessToken
		acc.WorkBuddyRefreshToken = refreshed.RefreshToken
		if !refreshed.ExpiresAt.IsZero() {
			acc.WorkBuddyExpiresAt = refreshed.ExpiresAt
		}
		if refreshed.UID != "" {
			acc.WorkBuddyUID = refreshed.UID
		}
	}
	if !NormalizeCredentials(acc) {
		return nil, ErrMissingCredential
	}
	return acc, nil
}
