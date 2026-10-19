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

// loginInterval is the upstream poll cadence. The CLI polls every second; the
// console polls a little slower so a stalled transaction cannot hammer the
// token endpoint for fifteen minutes.
const loginInterval = 2 * time.Second

// loginPort is the official Qoder device authorization flow.
//
// Only the challenge and the official page URL ever reach the browser. The
// verifier, the nonce and the resulting credential stay server-side, so a
// compromised browser session cannot mint a credential from a half-finished
// transaction.
func loginPort() *provider.Login {
	return &provider.Login{
		UnreachableDetail: "the server cannot reach the Qoder authorization endpoint",
		ErrPending:        ErrAuthPending,
		ErrUnavailable:    ErrAuthUnavailable,
		ErrRejected:       ErrAuthRejected,
		// The upstream issues the device code during the first poll rather than
		// at start, so a transaction is pollable without one.
		NeedsDeviceCode: false,
		Start: func(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
			client := NewFromAccount(nil, cfg)
			defer client.Close()
			transaction, err := client.StartLogin(ctx)
			if err != nil {
				return provider.LoginChallenge{}, err
			}
			return provider.LoginChallenge{
				VerifyURI:  loginVerifyURI(cfg),
				VerifyFull: transaction.VerifyURL,
				ExpiresAt:  transaction.ExpiresAt,
				Interval:   loginInterval,
				// The verifier and the nonce stay in the private half: the rest
				// of the challenge is what the polling response is built from,
				// and a nonce the browser could read would let a third party
				// complete the transaction.
				Private: transaction,
			}, nil
		},
		Exchange: func(ctx context.Context, cfg *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
			transaction, ok := challenge.Private.(*LoginTransaction)
			if !ok {
				return nil, errors.New("qoder login transaction is missing")
			}
			client := NewFromAccount(nil, cfg)
			defer client.Close()
			return client.PollLogin(ctx, &LoginTransaction{
				VerifyURL: transaction.VerifyURL, Nonce: transaction.Nonce, Verifier: transaction.Verifier,
				MachineID: transaction.MachineID, ExpiresAt: challenge.ExpiresAt,
			})
		},
		Build: func(ctx context.Context, cfg *config.Config, loginID string, challenge provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
			transaction, ok := challenge.Private.(*LoginTransaction)
			creds, isCreds := grant.(Credentials)
			if !ok || !isCreds {
				return nil, errors.New("qoder login produced an unexpected grant")
			}
			return BuildLoginAccount(ctx, cfg, loginID, transaction.MachineID, creds)
		},
		MarkReplace: func(acc *store.Account) { acc.ReplaceQoderCredentials = true },
	}
}

// BuildLoginAccount turns a completed login into an account record.
//
// What makes a login succeed is the device credential plus a resolved identity:
// the upstream issued the token and named the account, and both are durable. The
// model list read is enrichment, not a credential check, so a gateway that does
// not answer it must not cost the operator a valid credential. The catalog is
// read from the signed control plane; when that read fails the snapshot is left
// empty, the real reason is logged, and the operator runs a model refresh.
func BuildLoginAccount(ctx context.Context, cfg *config.Config, loginID, machineID string, creds Credentials) (*store.Account, error) {
	normalized := NormalizeLoginResult(creds, machineID)
	acc := &store.Account{
		Name:              "qoder-login",
		AccountType:       "qoder",
		QoderAccessToken:  normalized.AccessToken,
		QoderRefreshToken: normalized.RefreshToken,
		QoderExpiresAt:    normalized.AccessExpiresAt,
		QoderMachineID:    normalized.MachineID,
		QoderUserID:       normalized.UID,
		QoderUserName:     normalized.Name,
		QoderDataPolicy:   true,
		Weight:            1,
		Enabled:           true,
	}
	acc.Email = util.FirstNonEmpty(normalized.Email, acc.Email)

	client := NewFromAccount(acc, cfg)
	defer client.Close()
	// The ID-zero client cannot persist token rotations itself. Copy its final
	// credential and runtime state before the account is returned for creation.
	defer client.CopyAccountState(acc)

	accessToken, err := client.CurrentAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	// The identity enrichment is optional: a userinfo outage must not invalidate
	// a credential that already works.
	if profile, err := client.FetchProfile(ctx, accessToken); err != nil {
		slog.Debug("Qoder profile lookup failed; keeping the login identity", "login_id", loginID, "error", err)
	} else {
		profile.ApplyToAccount(acc)
		client.ApplyProfile(profile)
	}

	if strings.TrimSpace(acc.QoderUserID) == "" {
		return nil, errors.New("qoder login returned no user id")
	}
	switch {
	case strings.TrimSpace(acc.Email) != "":
		acc.Name = acc.Email
	case strings.TrimSpace(acc.QoderUserName) != "":
		acc.Name = acc.QoderUserName
	case len(acc.QoderUserID) >= 8:
		acc.Name = "qoder-" + acc.QoderUserID[:8]
	}

	// Derive the runtime auth pair now so the account is usable and the failure
	// surface is the login rather than the first chat request.
	if err := client.PrepareCurrentRuntimeFields(ctx); err != nil {
		return nil, err
	}
	acc.QoderRuntimeInfo = client.RuntimeFields().EncryptUserInfo
	acc.QoderRuntimeKey = client.RuntimeFields().Key

	// The catalog is read from the signed upstream control plane at login. A
	// failed read leaves the snapshot empty on purpose: the account is saved and
	// a later refresh records the catalog, but nothing compiled in is installed
	// as if it had been observed.
	if models, catalogErr := client.FetchUpstreamModels(ctx); catalogErr != nil {
		slog.Warn("Qoder upstream catalog read failed at login; leaving the snapshot empty",
			"login_id", loginID, "error", catalogErr)
	} else {
		acc.QoderModelIDs = CatalogSnapshot(models)
		if len(acc.QoderModelIDs) > 0 {
			acc.QoderModelsSyncedAt = time.Now()
		}
	}

	// Record the allowance at login so the account table can show the plan and
	// the operator learns immediately whether this account can actually spend.
	// A failure here is informational: the credential is already good.
	if quota, quotaErr := client.FetchQuota(ctx); quotaErr != nil {
		slog.Warn("Qoder quota read failed at login; leaving the allowance unknown",
			"login_id", loginID, "error", quotaErr)
	} else {
		ApplyQuota(acc, quota)
		if quota.Exhausted {
			acc.StatusCode = store.AccountStatusQoderQuotaExhausted
			acc.StatusMessage = "Qoder allowance exhausted; free catalog models remain eligible"
			acc.LastAttempt = time.Now()
		}
	}

	if err := client.FinalizeAccountState(ctx, acc); err != nil {
		return nil, err
	}
	if strings.TrimSpace(acc.QoderRefreshToken) == "" {
		// Without a refresh token the account cannot survive its first token
		// expiry, and the login would look successful until it silently dies.
		return nil, errors.New("qoder login returned no refresh token")
	}
	return acc, nil
}

// loginVerifyURI is the human-facing authorization page. It is what the console
// shows as a link, while verifyFull carries the single-use challenge.
func loginVerifyURI(cfg *config.Config) string {
	if cfg != nil {
		if base := strings.TrimSpace(cfg.QoderOAuthBaseURL); base != "" {
			return strings.TrimRight(base, "/") + "/device/selectAccounts"
		}
	}
	return DefaultOAuthBaseURL + "/device/selectAccounts"
}
