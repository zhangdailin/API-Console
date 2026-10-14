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
			if uid := strings.TrimSpace(acc.WorkBuddyUID); uid != "" {
				return "workbuddy:uid:" + uid
			}
			return ""
		},
		Fingerprint: func(acc *store.Account) string {
			creds := ResolveCredentials(acc)
			return util.Fingerprint(util.FirstNonEmpty(creds.AccessToken, creds.RefreshToken))
		},
		PrepareCreate: func(acc *store.Account) error {
			if !NormalizeCredentials(acc) {
				return errors.New("missing WorkBuddy credential: paste the access token or refresh token from the WorkBuddy desktop session")
			}
			return nil
		},
		PrepareUpdate: prepareUpdate,
		PrepareImport: func(acc *store.Account) string {
			if !NormalizeCredentials(acc) || strings.TrimSpace(acc.WorkBuddyRefreshToken) == "" {
				return "missing_refresh_token"
			}
			return ""
		},
		// The durable refresh token never leaves the server; the access token
		// stays on the copy only so the fingerprint and the "has a credential"
		// flag can be derived before the response drops it.
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

// prepareUpdate applies an edit from the account form. The read path redacts
// the refresh token, so an ordinary edit arrives without it; the stored
// credential is kept unless a new one was actually submitted.
func prepareUpdate(acc, existing *store.Account) error {
	submitted := ResolveCredentials(acc)
	PreserveCredentialsOnEdit(acc, existing)
	if current := ResolveCredentials(acc); current.RefreshToken == "" && current.AccessToken == "" {
		return errors.New("missing WorkBuddy credential")
	}
	NormalizeCredentials(acc)
	existingCreds := ResolveCredentials(existing)
	acc.ReplaceWorkBuddyCredentials = submitted.HasCredential() &&
		(submitted.AccessToken != existingCreds.AccessToken || submitted.RefreshToken != existingCreds.RefreshToken)
	if acc.ReplaceWorkBuddyCredentials {
		acc.ClearVerifiedAt = true
	}
	return nil
}

// ErrMissingCredential is returned when neither a refresh token nor an
// access token was supplied.
var ErrMissingCredential = errors.New("workbuddy account is missing credentials")

// The WorkBuddy international channel keeps its durable refresh token out of
// the generic RefreshToken slot: the admin UI must never receive it, and an
// ordinary edit that omits the token field must not wipe it.

// NormalizeCredentials stores a newly submitted credential in the
// WorkBuddy fields. The identity (UID / signed-in address) comes from the
// access-token JWT's Keycloak claims when available.
func NormalizeCredentials(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	creds := ResolveCredentials(acc)
	if !creds.HasCredential() {
		return false
	}
	accessToken, refreshToken, uid, email, expiresAt := creds.Fields()
	acc.WorkBuddyAccessToken = accessToken
	if refreshToken != "" {
		acc.WorkBuddyRefreshToken = refreshToken
	}
	if uid != "" {
		acc.WorkBuddyUID = uid
	}
	if email != "" {
		acc.Email = email
		if strings.TrimSpace(acc.Name) == "" {
			// The desktop client's nickname is the signed-in address.
			acc.Name = email
		}
	}
	if !expiresAt.IsZero() {
		acc.WorkBuddyExpiresAt = expiresAt
	}
	// The WorkBuddy credential lives in its own fields only: the generic slots
	// are shared with channels whose credentials have different semantics, and
	// writing a JWT there would leak it through the account list.
	acc.Token = ""
	acc.RefreshToken = ""
	acc.ClientCookie = ""
	return true
}

// PreserveCredentialsOnEdit keeps server-side credentials when the
// admin UI submits an edit without re-entering the token (secrets are redacted
// on read, so an empty field means "keep").
func PreserveCredentialsOnEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil {
		return
	}
	provider.PreserveBlank(&acc.WorkBuddyAccessToken, existing.WorkBuddyAccessToken)
	provider.PreserveBlank(&acc.WorkBuddyRefreshToken, existing.WorkBuddyRefreshToken)
	provider.PreserveBlank(&acc.WorkBuddyUID, existing.WorkBuddyUID)
	provider.PreserveTime(&acc.WorkBuddyExpiresAt, existing.WorkBuddyExpiresAt)
	if !isFullCatalog(acc.WorkBuddyModelIDs) {
		// An edit must not erase the account-scoped catalog snapshot, and the UI
		// has no way to submit it.
		acc.WorkBuddyModelIDs = append([]string(nil), existing.WorkBuddyModelIDs...)
		acc.WorkBuddyModelsSyncedAt = existing.WorkBuddyModelsSyncedAt
	}
	if acc.WorkBuddyQuota.SyncedAt.IsZero() {
		acc.WorkBuddyQuota = existing.WorkBuddyQuota
	}
	// Provider-observed usage and health are not editable through the account
	// form; a partial PUT must not zero the credit meter the table displays.
	provider.PreserveObservedState(acc, existing)
}

// isFullCatalog reports whether a snapshot carries a plausible catalog.
// The admin API never accepts the snapshot from a client, so this is a guard
// against an accidental partial overwrite rather than a validation rule.
func isFullCatalog(ids []string) bool { return len(ids) >= 4 }

// RedactOutput hides the durable refresh token and unrelated legacy
// secrets while leaving the access token visible (the management UI shows a
// truncated form so an operator can tell whether a credential is configured).
func RedactOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	out.WorkBuddyRefreshToken = ""
	out.RefreshToken = ""
	out.Token = ""
	return &out
}

// CredentialKey identifies an account by its durable credential so the
// duplicate detector can reject the same account twice.
func CredentialKey(acc *store.Account) string {
	creds := ResolveCredentials(acc)
	for _, candidate := range []string{creds.RefreshToken, creds.AccessToken} {
		if strings.TrimSpace(candidate) != "" {
			return "workbuddy:" + candidate
		}
	}
	return ""
}

// VerifyAccount proves the credential works before it is persisted and
// applies the account-scoped model catalog and credit meter on the way.
func VerifyAccount(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore AccountUpdater) (string, int, error) {
	if acc == nil {
		return "", 0, nil
	}
	client := NewFromAccount(acc, cfg)
	defer client.Close()
	if accountStore != nil {
		client.SetAccountStore(accountStore)
	}

	// Identity first: a credential added before this channel stored the claims (or
	// pasted as a raw session document) still resolves to a UID and an address, so
	// the account/email column fills in on the very next sync.
	creds := ResolveCredentials(acc)
	if uid, _, email, _, _ := creds.Fields(); uid != "" || email != "" {
		if acc.WorkBuddyUID == "" {
			acc.WorkBuddyUID = uid
		}
		if strings.TrimSpace(acc.Email) == "" {
			acc.Email = email
		}
		if strings.TrimSpace(acc.Name) == "" && email != "" {
			acc.Name = email
		}
	}
	if !creds.HasCredential() {
		return "", 400, ErrMissingCredential
	}

	models, err := client.FetchModels(ctx)
	if err != nil {
		return "", 502, err
	}

	ids := CatalogSnapshot(models)
	if len(ids) > 0 {
		acc.WorkBuddyModelIDs = ids
		acc.WorkBuddyModelsSyncedAt = time.Now()
	}

	// The credit meter is a separate, optional endpoint. A failure must not turn
	// an otherwise usable account into an error; it only leaves the quota
	// unavailable until the next sync.
	if quota, quotaErr := client.FetchQuota(ctx); quotaErr != nil {
		slog.Warn("WorkBuddy credit meter sync failed; leaving quota unavailable",
			"account_id", acc.ID, "error", quotaErr)
	} else {
		ApplyQuota(acc, quota)
		if acc.UsageLimit > 0 && acc.UsageCurrent <= 0 {
			// A spent metered package narrows this account to the confirmed free
			// catalog rather than parking it. The model-aware selector enforces the
			// intersection with this account's latest advertised model snapshot.
			return store.AccountStatusWorkBuddyQuotaExhausted, 0, nil
		}
	}
	return "", 0, nil
}

// The meter reports the remaining credits of the current cycle; the
// generic UsageCurrent slot stores that remaining value for this channel,
// so "used" must be derived rather than read from UsageCurrent.
func projectQuota(fields map[string]any, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	snapshot := acc.WorkBuddyQuota
	quotaLimit := limit
	if snapshot.Limit > 0 {
		quotaLimit = snapshot.Limit
	}
	quotaRemaining := current
	if !snapshot.SyncedAt.IsZero() {
		quotaRemaining = snapshot.Remaining
	}
	if quotaLimit <= 0 {
		fields["quota_limit"] = 0.0
		fields["quota_used"] = 0.0
		fields["quota_remaining"] = 0.0
		fields["quota_mode"] = "unknown"
		fields["quota_unit"] = "credits"
		fields["quota_supported"] = false
		fields["quota_plan"] = snapshot.PackageName
		provider.QuotaProvenance(fields, "unknown", "upstreamBilling", "",
			"WorkBuddy 计量接口未返回额度", false, !snapshot.SyncedAt.IsZero())
		return
	}
	if quotaRemaining < 0 {
		quotaRemaining = 0
	}
	if quotaRemaining > quotaLimit {
		quotaRemaining = quotaLimit
	}
	used := snapshot.Used
	if snapshot.SyncedAt.IsZero() {
		used = quotaLimit - quotaRemaining
	}
	if used < 0 {
		used = 0
	}
	fields["quota_limit"] = quotaLimit
	fields["quota_used"] = used
	fields["quota_remaining"] = quotaRemaining
	fields["quota_mode"] = "remaining"
	fields["quota_unit"] = util.FirstNonEmpty(snapshot.Unit, "credits")
	fields["quota_supported"] = !snapshot.SyncedAt.IsZero()
	fields["quota_plan"] = snapshot.PackageName
	fields["quota_consumed_units"] = snapshot.LastConsumedUnits
	fields["quota_package_remaining"] = snapshot.PackageRemaining
	confidence := ""
	if !snapshot.SyncedAt.IsZero() {
		confidence = "confirmed"
	}
	provider.QuotaProvenance(fields, "paid", "upstreamBilling", confidence,
		"WorkBuddy 计量包返回的周期额度", quotaLimit > 0, !snapshot.SyncedAt.IsZero())
	if !snapshot.ResyncAt().IsZero() {
		fields["quota_reset_at"] = snapshot.ResyncAt().UTC().Format(time.RFC3339)
	}
}
