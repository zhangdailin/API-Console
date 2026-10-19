package qoder

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// adminPort is what the management API needs from this channel.
func adminPort() *provider.Admin {
	return &provider.Admin{
		CredentialKey: CredentialKey,
		IdentityKey: func(acc *store.Account) string {
			if acc == nil {
				return ""
			}
			if uid := strings.TrimSpace(acc.QoderUserID); uid != "" {
				return "qoder:uid:" + uid
			}
			if machineID := strings.TrimSpace(acc.QoderMachineID); machineID != "" {
				return "qoder:machine:" + machineID
			}
			return ""
		},
		Fingerprint: func(acc *store.Account) string {
			creds := ResolveCredentials(acc)
			return util.Fingerprint(util.FirstNonEmpty(creds.RefreshToken, creds.AccessToken))
		},
		// Qoder is OAuth-only: an account is created by the browser device flow
		// (/api/qoder/login), never by pasting a personal access token.
		PrepareCreate: func(*store.Account) error {
			return errors.New("Qoder accounts must be added using the official browser login (/api/qoder/login)")
		},
		PrepareUpdate: prepareUpdate,
		PrepareImport: func(acc *store.Account) string {
			if !NormalizeCredentials(acc) || strings.TrimSpace(acc.QoderRefreshToken) == "" {
				return "missing_refresh_token"
			}
			if strings.TrimSpace(acc.QoderMachineID) == "" {
				return "missing_device_identity"
			}
			return ""
		},
		// The durable refresh token and the derived runtime material never
		// leave the server.
		Project:      func(out *store.Account) { *out = *RedactOutput(out) },
		ProjectQuota: projectQuota,
		Verify: func(ctx context.Context, env provider.Env, acc *store.Account) (string, int, error) {
			if env.Store == nil {
				return VerifyAccount(ctx, acc, env.Config, nil)
			}
			return VerifyAccount(ctx, acc, env.Config, env.Store)
		},
		ErrMissingCredential: ErrMissingCredential,
		Login:                loginPort(),
	}
}

// prepareUpdate applies an edit from the account form, keeping the stored
// credential and device identity unless new ones were actually submitted.
func prepareUpdate(acc, existing *store.Account) error {
	submitted := ResolveCredentials(acc)
	submittedMachineID := strings.TrimSpace(acc.QoderMachineID)
	PreserveCredentialsOnEdit(acc, existing)
	if !NormalizeCredentials(acc) {
		return errors.New("missing Qoder credential: sign in again with the browser login")
	}
	if strings.TrimSpace(acc.QoderMachineID) == "" {
		return errors.New("missing Qoder device identity: sign in again")
	}
	existingCreds := ResolveCredentials(existing)
	acc.ReplaceQoderCredentials = submitted.HasCredential() &&
		(submitted.AccessToken != existingCreds.AccessToken ||
			submitted.RefreshToken != existingCreds.RefreshToken ||
			(submittedMachineID != "" && submittedMachineID != strings.TrimSpace(existing.QoderMachineID)))
	if acc.ReplaceQoderCredentials {
		acc.ClearVerifiedAt = true
	}
	return nil
}

// ErrMissingCredential is returned when an account carries no device
// credential.
var ErrMissingCredential = errors.New("qoder account is missing an OAuth credential")

// The Qoder channel is OAuth-only: an account is created by the device
// authorization flow, never by pasting a personal access token. The device
// refresh token is the durable credential and is rotated by the upstream, so it
// is kept out of the generic RefreshToken slot and is never returned by the
// account API.

// CredentialKey identifies an account by its durable credential so the
// duplicate detector can reject the same account twice.
func CredentialKey(acc *store.Account) string {
	creds := ResolveCredentials(acc)
	for _, candidate := range []string{creds.RefreshToken, creds.AccessToken} {
		if strings.TrimSpace(candidate) != "" {
			return "qoder:" + candidate
		}
	}
	return ""
}

// RedactOutput hides the durable refresh token and the derived runtime
// material. The access token stays visible in truncated form so an operator can
// tell that a credential is configured without either secret leaving the
// server.
func RedactOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	out.QoderRefreshToken = ""
	out.QoderRuntimeInfo = ""
	out.QoderRuntimeKey = ""
	out.QoderRuntimeIdentity = ""
	// The generic slots are shared with other channels; a Qoder record never
	// writes them, and clearing them here keeps a legacy row from leaking.
	out.RefreshToken = ""
	out.Token = ""
	out.ClientCookie = ""
	return &out
}

// PreserveCredentialsOnEdit keeps the server-side credential and derived
// material when the admin UI submits an edit without re-entering them (secrets
// are redacted on read, so an empty field means "keep").
func PreserveCredentialsOnEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil {
		return
	}
	provider.PreserveBlank(&acc.QoderAccessToken, existing.QoderAccessToken)
	provider.PreserveBlank(&acc.QoderRefreshToken, existing.QoderRefreshToken)
	provider.PreserveBlank(&acc.QoderMachineID, existing.QoderMachineID)
	provider.PreserveBlank(&acc.QoderUserID, existing.QoderUserID)
	provider.PreserveBlank(&acc.QoderUserName, existing.QoderUserName)
	provider.PreserveBlank(&acc.QoderOrganizationID, existing.QoderOrganizationID)
	if len(acc.QoderOrganizationTags) == 0 {
		acc.QoderOrganizationTags = append([]string(nil), existing.QoderOrganizationTags...)
	}
	provider.PreserveTime(&acc.QoderExpiresAt, existing.QoderExpiresAt)
	provider.PreserveBlank(&acc.QoderRuntimeInfo, existing.QoderRuntimeInfo)
	provider.PreserveBlank(&acc.QoderRuntimeKey, existing.QoderRuntimeKey)
	provider.PreserveBlank(&acc.QoderRuntimeIdentity, existing.QoderRuntimeIdentity)
	// The identity in the runtime plaintext is immutable for a credential: an
	// edit must not change the UID without re-deriving, or every request would
	// be signed with material that no longer matches the account.
	if strings.TrimSpace(existing.QoderUserID) != "" {
		acc.QoderUserID = existing.QoderUserID
	}
	// The catalog snapshot and its provenance are not editable through the form.
	if len(acc.QoderModelIDs) == 0 {
		acc.QoderModelIDs = append([]string(nil), existing.QoderModelIDs...)
	}
	provider.PreserveTime(&acc.QoderModelsSyncedAt, existing.QoderModelsSyncedAt)
	// Provider-observed usage and health are not editable either.
	provider.PreserveObservedState(acc, existing)
	provider.PreserveTime(&acc.VerifiedAt, existing.VerifiedAt)
}

// NormalizeCredentials binds the account's device identity to its
// credential. A credential without a machine id cannot sign a request: the
// upstream rejects a device identity that did not perform the authorization.
func NormalizeCredentials(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	creds := ResolveCredentials(acc)
	if !creds.HasCredential() {
		return false
	}
	accessToken, refreshToken, expiresAt, uid, name, email, orgID, orgTags := creds.Fields()
	acc.QoderAccessToken = accessToken
	if refreshToken != "" {
		acc.QoderRefreshToken = refreshToken
	}
	if !expiresAt.IsZero() {
		acc.QoderExpiresAt = expiresAt
	}
	if uid != "" {
		acc.QoderUserID = uid
	}
	if name != "" {
		acc.QoderUserName = name
	}
	if email != "" {
		acc.Email = email
	}
	if orgID != "" {
		acc.QoderOrganizationID = orgID
	}
	if len(orgTags) > 0 {
		acc.QoderOrganizationTags = orgTags
	}
	// The credential lives in its own fields only: the generic slots are shared
	// with channels whose credentials mean something else, and writing a Qoder
	// token there would leak it through another channel's account list.
	acc.Token = ""
	acc.RefreshToken = ""
	acc.ClientCookie = ""
	return true
}

// VerifyAccount checks that a Qoder account is still usable and refreshes
// the identity and derived material it owns.
//
// Scope is deliberately credential liveness only. The model list is a separate
// control-plane read: it is readable with the same COSY signature the chat call
// uses, but a gateway that stops answering it (or answers a shape this channel
// cannot parse) must not be reported as a broken credential. Entitlement is not
// checked here either: a plan problem is reported by the chat call itself, in
// terms the operator can act on, and marking the credential dead over it is
// exactly the mistake being corrected.
func VerifyAccount(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore AccountUpdater) (string, int, error) {
	if acc == nil {
		return "", 0, nil
	}
	creds := ResolveCredentials(acc)
	if !creds.HasCredential() {
		return "", 400, ErrMissingCredential
	}
	if strings.TrimSpace(acc.QoderMachineID) == "" {
		return "", 400, errors.New("qoder account is missing its device identity; sign in again")
	}

	client := NewFromAccount(acc, cfg)
	defer client.Close()
	// Later whole-account saves must not replay an earlier credential over an
	// in-flight rotation persisted by the client.
	defer client.CopyAccountState(acc)
	if accountStore != nil {
		client.SetAccountStore(accountStore)
	}

	accessToken, err := client.CurrentAccessToken(ctx)
	if err != nil {
		return "", 502, err
	}
	// The runtime pair encrypts the UID, so it cannot be produced before the
	// identity is known. A credential imported without one is completed here.
	if profile, err := client.FetchProfile(ctx, accessToken); err == nil {
		profile.ApplyToAccount(acc)
		client.ApplyProfile(profile)
	} else {
		slog.Debug("Qoder profile lookup failed during verification", "account_id", acc.ID, "error", err)
	}
	if strings.TrimSpace(acc.QoderUserID) == "" {
		return "", 400, errors.New("qoder account has no user id; sign in again")
	}

	if err := client.PrepareCurrentRuntimeFields(ctx); err != nil {
		return "", 502, err
	}
	fields := client.RuntimeFields()
	acc.QoderRuntimeInfo = fields.EncryptUserInfo
	acc.QoderRuntimeKey = fields.Key
	acc.QoderRuntimeIdentity = fields.Identity

	// The catalog comes from the signed upstream control plane. A failed read
	// leaves the snapshot untouched rather than installing a compiled-in list, so
	// an unreadable catalog stays visible as "not observed yet".
	if models, catalogErr := client.FetchUpstreamModels(ctx); catalogErr != nil {
		slog.Warn("Qoder catalog read failed; leaving the observed snapshot unchanged",
			"account_id", acc.ID, "error", catalogErr)
	} else if ids := CatalogSnapshot(models); len(ids) > 0 {
		acc.QoderModelIDs = ids
		acc.QoderModelsSyncedAt = time.Now()
	}

	// The credit/plan read is what distinguishes "this credential is broken" from
	// "this account has nothing left to spend". It is a separate OpenAPI call, so
	// a failure only leaves the quota unavailable.
	if quota, quotaErr := client.FetchQuota(ctx); quotaErr != nil {
		slog.Warn("Qoder quota sync failed; leaving the allowance unknown",
			"account_id", acc.ID, "error", quotaErr)
		if acc.QoderQuota.Exhausted || acc.StatusCode == store.AccountStatusQoderQuotaExhausted {
			if err := client.FinalizeAccountState(ctx, acc); err != nil {
				return "", 502, err
			}
			return store.AccountStatusQoderQuotaExhausted, 0, nil
		}
	} else {
		ApplyQuota(acc, quota)
		if quota.Exhausted {
			// Quota exhaustion is a capability downgrade when this account's
			// current catalog contains an explicitly zero-factor model. The
			// request selector admits only that free model; if the catalog has no
			// such row the account matches no request and stays effectively parked.
			if err := client.FinalizeAccountState(ctx, acc); err != nil {
				return "", 502, err
			}
			return store.AccountStatusQoderQuotaExhausted, 0, nil
		}
	}
	if err := client.FinalizeAccountState(ctx, acc); err != nil {
		return "", 502, err
	}
	return "", 0, nil
}

// The gateway reports the credit window directly, including its own
// exhausted verdict. The numbers are authoritative when a snapshot exists,
// and the verdict decides whether the account can spend at all — so the
// console can say "Free, 0 credits left, upgrade here" instead of showing
// a broken account.
func projectQuota(fields map[string]any, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	snapshot := acc.QoderQuota
	windowLimit := snapshot.Limit
	if windowLimit <= 0 {
		windowLimit = snapshot.LastKnownLimit
	}
	remaining := snapshot.Remaining
	if remaining < 0 {
		remaining = 0
	}
	used := snapshot.Used
	if used < 0 {
		used = 0
	}
	fields["quota_limit"] = windowLimit
	fields["quota_used"] = used
	fields["quota_remaining"] = remaining
	fields["quota_mode"] = "remaining"
	fields["quota_unit"] = util.FirstNonEmpty(snapshot.Unit, "credits")
	fields["quota_supported"] = !snapshot.SyncedAt.IsZero()
	fields["quota_plan"] = snapshot.PlanTier
	provider.NamedPlanProjection(fields, snapshot.PlanTier, !snapshot.SyncedAt.IsZero())
	fields["quota_exhausted"] = snapshot.Exhausted
	fields["quota_upgrade_url"] = snapshot.UpgradeURL
	fields["quota_reset_at"] = snapshot.ResetAt
	fields["quota_pools"] = map[string]store.QoderCreditPool{
		"plan":         snapshot.PlanQuota,
		"add_on":       snapshot.AddOnQuota,
		"organization": snapshot.OrgQuota,
	}
	// The gateway reports the window total itself, so a snapshot's windowLimit is
	// known even when it is zero (a Free plan with nothing left). Passing
	// limitKnown=false here would make consumers treat a reported window as an
	// estimate.
	if snapshot.Exhausted {
		// The allowance is spent: the credit window is the reason, and the
		// numbers are still reported rather than hidden.
		provider.QuotaProvenance(fields, "upstreamQuota", "upstreamUsage", "", "", true, true)
		return
	}
	if snapshot.SyncedAt.IsZero() {
		provider.QuotaProvenance(fields, "unknown", "upstreamUsage", "",
			"Qoder 额度接口未返回数据", false, false)
		return
	}
	provider.QuotaProvenance(fields, "upstreamQuota", "upstreamUsage", "", "", true, true)
}
