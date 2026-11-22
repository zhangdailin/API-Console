package warp

import (
	"context"
	"errors"
	"strings"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

var ErrMissingCredential = errors.New("warp account is missing an official login credential")

func adminPort() *provider.Admin {
	return &provider.Admin{
		CredentialKey: func(acc *store.Account) string {
			if acc == nil || acc.WarpRefreshToken == "" {
				return ""
			}
			return "warp:" + acc.WarpRefreshToken
		},
		IdentityKey: func(acc *store.Account) string {
			if acc == nil {
				return ""
			}
			if acc.UserID != "" {
				return "warp:user:" + acc.UserID
			}
			if acc.Email != "" {
				return "warp:email:" + strings.ToLower(acc.Email)
			}
			return ""
		},
		Fingerprint: func(acc *store.Account) string {
			if acc == nil {
				return ""
			}
			return util.Fingerprint(acc.WarpRefreshToken)
		},
		PrepareCreate: func(*store.Account) error {
			return errors.New("Warp accounts must be added using the official browser login")
		},
		PrepareImport: func(*store.Account) string { return "official_login_required" },
		PrepareUpdate: func(acc, existing *store.Account) error {
			if externalCredentialSubmitted(acc) || !acc.WarpExpiresAt.IsZero() {
				return errors.New("Warp credentials can only be changed using the official browser login")
			}
			applyCredentials(acc, credentials(existing))
			acc.Email = existing.Email
			acc.UserID = existing.UserID
			copyCatalogSnapshot(acc, existing)
			acc.WarpQuota = existing.WarpQuota.Clone()
			provider.PreserveObservedState(acc, existing)
			return nil
		},
		Project: func(acc *store.Account) {
			acc.WarpAccessToken = ""
			acc.WarpRefreshToken = ""
			acc.WarpExpiresAt = time.Time{}
			acc.ReplaceWarpCredentials = false
			acc.Token = ""
			acc.RefreshToken = ""
			acc.ClientCookie = ""
			acc.ClineAccessToken = ""
			acc.ClineRefreshToken = ""
			acc.OAuthAccessToken = ""
			acc.OAuthRefreshToken = ""
		},
		ProjectQuota: func(fields map[string]any, acc *store.Account, _, _ float64, _ int64, _ bool) {
			projectWarpQuota(fields, acc)
			provider.PlanProjection(fields, "", provider.PlanTierUnknown, "")
		},
		Verify: func(ctx context.Context, env provider.Env, acc *store.Account) (string, int, error) {
			if acc == nil || acc.WarpRefreshToken == "" {
				return "", 400, ErrMissingCredential
			}
			c := newClientWithStore(acc, env.Config, env.Store)
			observation, err := c.observeCatalog(ctx)
			if err != nil {
				return "", 502, err
			}
			observation.Record(acc, time.Now())
			c.refreshQuota(ctx, acc, env.Store)
			return "", 0, nil
		},
		ErrMissingCredential: ErrMissingCredential, Login: loginPort(),
	}
}
