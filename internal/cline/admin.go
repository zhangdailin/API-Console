package cline

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
			if email := strings.ToLower(strings.TrimSpace(acc.ClineEmail)); email != "" {
				return "cline:email:" + email
			}
			return ""
		},
		Fingerprint: func(acc *store.Account) string {
			creds := ResolveCredentials(acc)
			return util.Fingerprint(util.FirstNonEmpty(creds.RefreshToken, creds.AccessToken))
		},
		// Cline is OAuth-only: the WorkOS device grant is the only way to obtain
		// the credential.
		PrepareCreate: func(*store.Account) error {
			return errors.New("Cline accounts must be added using the official browser login (/api/cline/login)")
		},
		PrepareUpdate: prepareUpdate,
		PrepareImport: func(acc *store.Account) string {
			if !NormalizeCredentials(acc) || strings.TrimSpace(acc.ClineRefreshToken) == "" {
				return "missing_refresh_token"
			}
			return ""
		},
		// The durable refresh token never leaves the server.
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
	if !NormalizeCredentials(acc) {
		return errors.New("missing Cline credential: sign in again with the browser login")
	}
	existingCreds := ResolveCredentials(existing)
	acc.ReplaceClineCredentials = submitted.HasCredential() &&
		(submitted.AccessToken != existingCreds.AccessToken ||
			submitted.RefreshToken != existingCreds.RefreshToken)
	if acc.ReplaceClineCredentials {
		acc.ClearVerifiedAt = true
	}
	return nil
}

// ErrMissingCredential is returned when an account carries no Cline
// credential.
var ErrMissingCredential = errors.New("cline account is missing an OAuth credential")

// The Cline channel is OAuth-only: an account is created by the WorkOS device
// authorization flow, never by pasting a personal access token. The Cline
// refresh token is the durable credential and is rotated by the upstream, so it
// is kept out of the generic RefreshToken slot and is never returned by the
// account API.

// CredentialKey identifies an account by its durable credential so the
// duplicate detector can reject the same account twice.
func CredentialKey(acc *store.Account) string {
	creds := ResolveCredentials(acc)
	for _, candidate := range []string{creds.RefreshToken, creds.AccessToken} {
		if strings.TrimSpace(candidate) != "" {
			return "cline:" + candidate
		}
	}
	return ""
}

// RedactOutput hides the durable refresh token. The access token stays
// visible in truncated form so an operator can tell that a credential is
// configured without either secret leaving the server.
func RedactOutput(acc *store.Account) *store.Account {
	if acc == nil {
		return nil
	}
	out := *acc
	out.ClineRefreshToken = ""
	// The generic slots are shared with other channels; a Cline record never
	// writes them, and clearing them here keeps a legacy row from leaking.
	out.RefreshToken = ""
	out.Token = ""
	out.ClientCookie = ""
	return &out
}

// PreserveCredentialsOnEdit keeps the server-side credential when the admin
// UI submits an edit without re-entering it (secrets are redacted on read, so an
// empty field means "keep").
func PreserveCredentialsOnEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil {
		return
	}
	provider.PreserveBlank(&acc.ClineAccessToken, existing.ClineAccessToken)
	provider.PreserveBlank(&acc.ClineRefreshToken, existing.ClineRefreshToken)
	provider.PreserveTime(&acc.ClineExpiresAt, existing.ClineExpiresAt)
	provider.PreserveBlank(&acc.ClineEmail, existing.ClineEmail)
	if len(acc.ClineModelIDs) == 0 {
		acc.ClineModelIDs = append([]string(nil), existing.ClineModelIDs...)
		acc.ClineModelsSyncedAt = existing.ClineModelsSyncedAt
	}
}

// NormalizeCredentials validates a Cline record's credential.
//
// The credential lives in its own fields only: the generic slots are shared with
// channels whose credentials mean something else, and writing a Cline token
// there would leak it through another channel's account list.
func NormalizeCredentials(acc *store.Account) bool {
	if acc == nil {
		return false
	}
	creds := ResolveCredentials(acc)
	if !creds.HasCredential() {
		return false
	}
	accessToken, refreshToken, expiresAt, email := creds.Fields()
	acc.ClineAccessToken = accessToken
	if refreshToken != "" {
		acc.ClineRefreshToken = refreshToken
	}
	if !expiresAt.IsZero() {
		acc.ClineExpiresAt = expiresAt
	}
	if email != "" {
		acc.ClineEmail = email
	}
	acc.Token = ""
	acc.RefreshToken = ""
	acc.ClientCookie = ""
	return true
}

// VerifyAccount checks that a Cline account is still usable and refreshes
// the material it owns.
//
// Scope is deliberately credential liveness plus the account-scoped catalog.
// The model feed is a separate read on the same host, so a failure leaves the
// snapshot untouched rather than marking the credential dead: an account whose
// feed read fails is still able to run the models it last observed.
func VerifyAccount(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore AccountUpdater) (string, int, error) {
	if acc == nil {
		return "", 0, nil
	}
	creds := ResolveCredentials(acc)
	if !creds.HasCredential() {
		return "", 400, ErrMissingCredential
	}

	client := NewFromAccount(acc, cfg)
	defer client.Close()
	if accountStore != nil {
		client.SetAccountStore(accountStore)
	}

	observeAccount(ctx, client, acc, "account_id", acc.ID,
		"Cline catalog read failed; leaving the observed snapshot unchanged",
		"Cline plan read failed; leaving the recorded tier unchanged")

	// Renewing is the proof the credential still works: the refresh endpoint is
	// the only control-plane call that answers for a rotated token, and a
	// rejection there means the account has to be authorized again.
	if _, err := client.FetchModels(ctx); err != nil {
		return "", 502, err
	}
	return "", 0, nil
}

// Optional account decoration is shared by login and verification; verification
// still performs its separate renewal proof after this best-effort observation.
func observeAccount(ctx context.Context, client *Client, acc *store.Account, key string, id any, catalogMessage, planMessage string) {
	if models, err := client.FetchUpstreamModels(ctx); err != nil {
		slog.Warn(catalogMessage, key, id, "error", err)
	} else if ids := CatalogSnapshot(models); len(ids) > 0 {
		acc.ClineModelIDs = ids
		acc.ClineModelsSyncedAt = time.Now()
	}
	if plan, err := client.FetchPlan(ctx); err != nil {
		slog.Warn(planMessage, key, id, "error", err)
	} else if plan.Explicit {
		acc.ClinePlan = plan.Name
	}
}

// Cline publishes no numeric allowance: the free feed is a list, the inference
// cap is a rate limit expressed in prose, and there is no credit meter to read.
//
// The projection therefore answers "unsupported" instead of inventing a balance.
// Fabricating a limit here is what makes an account look spent when it is not,
// and the generic usage counters are reserved for channels that actually
// bill them.
func projectQuota(fields map[string]interface{}, acc *store.Account, limit, current float64, observedTokens int64, usageObserved bool) {
	fields["quota_limit"] = 0.0
	fields["quota_used"] = 0.0
	fields["quota_remaining"] = 0.0
	fields["quota_mode"] = "unmetered"
	fields["quota_unit"] = "requests"
	fields["quota_supported"] = false
	provider.QuotaProvenance(fields, "unknown", "unknown", "",
		"Cline 未下发数值额度；达到推理上限时按 429 冷却处理", false, false)
}
