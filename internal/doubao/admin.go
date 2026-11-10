package doubao

import (
	"context"
	"errors"
	"strings"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

var ErrMissingCookie = errors.New("doubao account is missing a QR login cookie")

func adminPort() *provider.Admin {
	return &provider.Admin{
		CredentialKey: func(acc *store.Account) string {
			if acc == nil || strings.TrimSpace(acc.ClientCookie) == "" {
				return ""
			}
			return "doubao:" + util.Fingerprint(acc.ClientCookie)
		},
		IdentityKey: func(acc *store.Account) string {
			if acc == nil || strings.TrimSpace(acc.UserID) == "" {
				return ""
			}
			return "doubao:user:" + strings.TrimSpace(acc.UserID)
		},
		Fingerprint: func(acc *store.Account) string {
			if acc == nil {
				return ""
			}
			return util.Fingerprint(acc.ClientCookie)
		},
		PrepareCreate: func(*store.Account) error { return errors.New("Doubao accounts must be added through QR login") },
		PrepareUpdate: func(acc, existing *store.Account) error {
			if existing == nil {
				return ErrMissingCookie
			}
			*acc = *existing.Clone()
			return nil
		},
		PrepareImport: func(*store.Account) string { return "qr_login_required" },
		Project: func(out *store.Account) {
			if out == nil {
				return
			}
			out.ClientCookie, out.Token, out.RefreshToken = "", "", ""
			out.DoubaoDeviceID, out.DoubaoWebID, out.DoubaoTeaUUID = "", "", ""
		},
		ProjectQuota: func(fields map[string]any, _ *store.Account, _, _ float64, _ int64, _ bool) {
			provider.QuotaProvenance(fields, "unknown", "doubao_web", "", "Doubao Web does not publish a numeric quota", false, false)
			provider.PlanProjection(fields, "", provider.PlanTierUnknown, "")
		},
		Verify: func(ctx context.Context, env provider.Env, acc *store.Account) (string, int, error) {
			client := NewFromAccount(acc, env.Config)
			info, err := client.AccountInfo(ctx, client.jar())
			if err != nil {
				if strings.Contains(err.Error(), "status=401") {
					return "401", 401, err
				}
				return "", 502, err
			}
			acc.UserID = info.UserID
			if info.Name != "" {
				acc.Name = info.Name
			}
			return "", 200, nil
		},
		ErrMissingCredential: ErrMissingCookie, Login: loginPort(),
	}
}

var _ = context.Background
var _ = config.Config{}
