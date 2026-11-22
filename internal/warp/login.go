package warp

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

var (
	ErrAuthPending     = errors.New("warp authorization pending")
	ErrAuthUnavailable = errors.New("warp authorization unavailable")
	ErrAuthRejected    = errors.New("warp authorization rejected")
	errSlowDown        = errors.New("warp authorization polling slowed")
)

type device struct {
	DeviceCode string `json:"device_code"`
	UserCode   string `json:"user_code"`
	VerifyURI  string `json:"verification_uri"`
	VerifyFull string `json:"verification_uri_complete"`
	ExpiresIn  int    `json:"expires_in"`
	Interval   int    `json:"interval"`
}

func validVerificationURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Hostname() == "app.warp.dev" || u.Hostname() == "warp.dev") && u.Fragment == ""
}
func loginPort() *provider.Login {
	return &provider.Login{
		UnreachableDetail: "the server cannot reach the Warp authorization endpoint",
		ErrPending:        ErrAuthPending, ErrUnavailable: ErrAuthUnavailable, ErrRejected: ErrAuthRejected, BuildVerifies: true,
		SlowDown: func(err error) bool { return errors.Is(err, errSlowDown) },
		Start: func(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
			c := NewFromAccount(nil, cfg)
			if err := c.ensureProjectConfig(ctx); err != nil {
				return provider.LoginChallenge{}, ErrAuthRejected
			}
			var d device
			if err := c.json(ctx, c.base+"/api/v1/oauth/device/auth", "application/x-www-form-urlencoded", []byte("client_id=warp-agent-cli"), "", &d); err != nil {
				if errors.Is(err, errTransport) {
					return provider.LoginChallenge{}, ErrAuthUnavailable
				}
				return provider.LoginChallenge{}, ErrAuthRejected
			}
			if d.DeviceCode == "" || d.UserCode == "" || !validVerificationURL(d.VerifyURI) || (d.VerifyFull != "" && !validVerificationURL(d.VerifyFull)) {
				return provider.LoginChallenge{}, ErrAuthRejected
			}
			if d.Interval < 1 {
				d.Interval = 2
			}
			if d.Interval > 60 {
				d.Interval = 60
			}
			if d.ExpiresIn < 1 {
				d.ExpiresIn = 600
			}
			if d.ExpiresIn > 900 {
				d.ExpiresIn = 900
			}
			return provider.LoginChallenge{DeviceCode: d.DeviceCode, UserCode: d.UserCode, VerifyURI: d.VerifyURI, VerifyFull: d.VerifyFull, ExpiresAt: time.Now().Add(time.Duration(d.ExpiresIn) * time.Second), Interval: time.Duration(d.Interval) * time.Second}, nil
		},
		Exchange: func(ctx context.Context, cfg *config.Config, ch provider.LoginChallenge) (provider.LoginGrant, error) {
			if ch.DeviceCode == "" || time.Now().After(ch.ExpiresAt) {
				return nil, ErrAuthRejected
			}
			c := NewFromAccount(nil, cfg)
			form := url.Values{"client_id": {"warp-agent-cli"}, "device_code": {ch.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
			var grant struct {
				AccessToken string `json:"access_token"`
			}
			if err := c.json(ctx, c.base+"/api/v1/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()), "", &grant); err != nil {
				if errors.Is(err, errSlowDown) {
					return nil, errors.Join(ErrAuthPending, errSlowDown)
				}
				return nil, err
			}
			if grant.AccessToken == "" {
				return nil, ErrAuthRejected
			}
			return c.exchangeFirebase(ctx, grant.AccessToken)
		},
		Build: func(ctx context.Context, cfg *config.Config, _ string, _ provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
			g, ok := grant.(Credentials)
			if !ok || g.AccessToken == "" || g.RefreshToken == "" {
				return nil, ErrAuthRejected
			}
			acc := &store.Account{AccountType: "warp", Name: "warp-login", Email: g.Email, UserID: g.UserID, Enabled: true, Weight: 1, CredentialType: "oauth"}
			if g.Email != "" {
				acc.Name = g.Email
			}
			applyCredentials(acc, g)
			c := NewFromAccount(acc, cfg)
			// The Firebase exchange already proved the credential. Catalog and
			// quota reads are display/maintenance observations: a transient
			// failure must not discard an otherwise successful login. The
			// maintenance loop retries both until they are observed.
			if choices, err := c.FetchModelChoices(ctx); err == nil {
				setCatalogChoices(acc, choices, time.Now())
			}
			c.refreshQuota(ctx, acc, nil)
			return acc, nil
		},
		MarkReplace: func(acc *store.Account) { acc.ReplaceWarpCredentials = true },
		Merge: func(existing, fresh *store.Account) {
			applyCredentials(existing, credentials(fresh))
			existing.ReplaceWarpCredentials = true
			existing.Email = fresh.Email
			existing.UserID = fresh.UserID
			copyCatalogSnapshot(existing, fresh)
			existing.WarpQuota = fresh.WarpQuota.Clone()
			existing.ClearVerifiedAt = true
			existing.AuthStatus = store.AccountAuthStatusActive
			existing.StatusCode, existing.StatusMessage = "", ""
			existing.LastAttempt = time.Time{}
			existing.QuotaResetAt = time.Time{}
		},
	}
}

func externalCredentialSubmitted(acc *store.Account) bool {
	return strings.TrimSpace(acc.WarpAccessToken) != "" || strings.TrimSpace(acc.WarpRefreshToken) != "" || acc.ReplaceWarpCredentials
}
