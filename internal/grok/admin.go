package grok

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// ErrMissingOAuthToken is returned for a Build account that holds neither an
// access token nor a refresh token.
var ErrMissingOAuthToken = errors.New("missing oauth token")

// adminPort is what the management API needs from this channel.
func adminPort() *provider.Admin {
	return &provider.Admin{
		CredentialKey: func(acc *store.Account) string {
			if acc == nil {
				return ""
			}
			token := strings.TrimSpace(util.FirstNonEmpty(acc.OAuthRefreshToken, acc.OAuthAccessToken))
			if token == "" {
				return ""
			}
			return "grok:" + token
		},
		IdentityKey: func(acc *store.Account) string {
			if !accountIsOAuth(acc) {
				return ""
			}
			if userID := strings.TrimSpace(acc.UserID); userID != "" {
				return "grok:oauth:user:" + userID
			}
			if email := strings.ToLower(strings.TrimSpace(acc.Email)); email != "" {
				return "grok:oauth:email:" + email
			}
			return ""
		},
		Fingerprint: func(acc *store.Account) string {
			if acc == nil {
				return ""
			}
			if accountIsOAuth(acc) {
				return util.Fingerprint(util.FirstNonEmpty(acc.OAuthAccessToken, acc.OAuthRefreshToken))
			}
			return util.Fingerprint(util.FirstNonEmpty(acc.ClientCookie, acc.RefreshToken, acc.Token))
		},
		PrepareCreate: func(acc *store.Account) error {
			normalizeTokenInput(acc)
			if !accountIsOAuth(acc) {
				return errors.New("Grok accounts must be added through the Build OAuth device login (/api/grok/device-auth)")
			}
			if !accountHasOAuthCredentials(acc) {
				return ErrMissingOAuthToken
			}
			return nil
		},
		PrepareUpdate: func(acc, existing *store.Account) error {
			normalizeTokenInput(acc)
			// Admin UI redacts OAuth secrets on read; empty inbound fields mean
			// "keep existing", not "clear credentials".
			preserveOAuthCredentials(acc, existing)
			preserveRuntimeStateOnAdminEdit(acc, existing)
			if accountIsOAuth(acc) && !accountHasOAuthCredentials(acc) {
				return ErrMissingOAuthToken
			}
			return nil
		},
		PrepareImport: func(acc *store.Account) string {
			normalizeTokenInput(acc)
			if strings.TrimSpace(acc.OAuthRefreshToken) == "" {
				return "missing_refresh_token"
			}
			return ""
		},
		Project:             projectOutput,
		ProjectQuota:        projectQuota,
		ObservedUsageWindow: FreeBuildUsageWindow,
		Verify: func(ctx context.Context, env provider.Env, acc *store.Account) (string, int, error) {
			err := verifyAccount(ctx, acc, env.Config, env.Store)
			if err == nil {
				return "", 0, nil
			}
			// The caller classifies the failure. One it cannot classify is an
			// upstream problem rather than a verdict on the credential.
			return "", http.StatusBadGateway, err
		},
		ErrMissingCredential: ErrMissingOAuthToken,
		Login:                loginPort(),
	}
}

// projectOutput shapes the copy of an account a response is built from.
func projectOutput(out *store.Account) {
	NormalizeProvider(out)
	// The tier column must agree with the quota column. A Build Free account
	// has no plan name from the identity endpoint (recorded as "unknown"), yet
	// the same Free inference that produces its quota window already proves it
	// is Free — and only Free. Reporting "unknown" there told an operator nothing
	// about an account the gateway had already characterised.
	if verdict := InferFreeProfile(out); verdict.Inferred {
		switch strings.ToLower(strings.TrimSpace(out.Subscription)) {
		case "", "unknown", "free":
			out.Subscription = "free"
		}
	}
	out.RefreshToken = ""
	// Never return the durable refresh token through normal account endpoints.
	out.OAuthRefreshToken = ""
}

func verifyAccount(ctx context.Context, acc *store.Account, cfg *config.Config, accountStore *store.Store) error {
	if acc == nil {
		return fmt.Errorf("missing grok account")
	}
	// Build CLI OAuth accounts verify against the CLI proxy with a Bearer token.
	if accountIsOAuth(acc) {
		if !accountHasOAuthCredentials(acc) {
			return ErrMissingOAuthToken
		}
		cliClient := NewCLIClient(cfg)
		cliClient.SetAccountStore(accountStore)
		result := RefreshBuildAccount(ctx, cliClient, acc, BuildRefreshOptions{
			Verify: true, VerifyTimeout: 20 * time.Second,
			Billing: true,
			Models:  true, ModelsTimeout: 15 * time.Second,
		})
		if result.VerifyErr != nil {
			if result.VerifyStatus != "" {
				return fmt.Errorf("%s: %w", result.VerifyStatus, result.VerifyErr)
			}
			return result.VerifyErr
		}
		if result.BillingErr != nil {
			slog.Warn("Grok CLI billing sync failed; leaving quota unavailable", "account_id", acc.ID, "error", result.BillingErr)
		}
		if result.ModelsErr != nil {
			slog.Warn("Grok CLI model catalog sync failed; keeping last catalog", "account_id", acc.ID, "error", result.ModelsErr)
		}
		return nil
	}

	// The website cookie plane was retired: only Build OAuth
	// credentials can be verified now.
	return fmt.Errorf("only Grok Build OAuth accounts are supported")
}

func normalizeTokenInput(acc *store.Account) {
	if acc == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	acc.CredentialType = "oauth"
	acc.GrokProvider = ProviderBuild
	acc.OAuthAccessToken = strings.TrimSpace(acc.OAuthAccessToken)
	acc.OAuthRefreshToken = strings.TrimSpace(acc.OAuthRefreshToken)
	// Non-Build Grok credentials must never survive an account write.
	acc.Token = ""
	acc.ClientCookie = ""
	acc.RefreshToken = ""
}

// accountIsOAuth reports whether a Grok account is a Build CLI OAuth account.
func accountIsOAuth(acc *store.Account) bool {
	return acc != nil && strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth")
}

// accountHasOAuthCredentials reports whether an OAuth account carries at
// least one usable token after normalization.
func accountHasOAuthCredentials(acc *store.Account) bool {
	if !accountIsOAuth(acc) {
		return false
	}
	return strings.TrimSpace(acc.OAuthAccessToken) != "" || strings.TrimSpace(acc.OAuthRefreshToken) != ""
}

// preserveOAuthCredentials keeps existing OAuth secrets when the admin UI
// submits empty fields (secrets are redacted on read and therefore absent on
// ordinary edit/save).
func preserveOAuthCredentials(acc, existing *store.Account) {
	if acc == nil || existing == nil || !accountIsOAuth(acc) {
		return
	}
	if strings.TrimSpace(acc.OAuthAccessToken) == "" {
		acc.OAuthAccessToken = existing.OAuthAccessToken
	}
	if strings.TrimSpace(acc.OAuthRefreshToken) == "" {
		acc.OAuthRefreshToken = existing.OAuthRefreshToken
	}
	if acc.OAuthExpiresAt.IsZero() && !existing.OAuthExpiresAt.IsZero() {
		acc.OAuthExpiresAt = existing.OAuthExpiresAt
	}
	if strings.TrimSpace(acc.TeamID) == "" {
		acc.TeamID = existing.TeamID
	}
}

// preserveRuntimeStateOnAdminEdit keeps provider-observed Build state out of
// the generic account edit surface. OAuth secrets are preserved separately.
func preserveRuntimeStateOnAdminEdit(acc, existing *store.Account) {
	if acc == nil || existing == nil || !strings.EqualFold(acc.AccountType, "grok") {
		return
	}
	acc.Token = existing.Token
	acc.Subscription = existing.Subscription
	acc.UsageCurrent = existing.UsageCurrent
	acc.UsageTotal = existing.UsageTotal
	acc.UsageLimit = existing.UsageLimit
	acc.StatusCode = existing.StatusCode
	acc.StatusMessage = existing.StatusMessage
	acc.LastAttempt = existing.LastAttempt
	acc.VerifiedAt = existing.VerifiedAt
	acc.QuotaResetAt = existing.QuotaResetAt
	acc.GrokModels = append([]string(nil), existing.GrokModels...)
	acc.GrokModelCatalog = modelcatalog.CloneProfiles(existing.GrokModelCatalog)
	acc.GrokModelsSyncedAt = existing.GrokModelsSyncedAt
	acc.GrokBilling = existing.GrokBilling
	acc.GrokRateLimits = existing.GrokRateLimits
}

// projectQuota projects one Build account's allowance.
//
// Upstream billing wins whenever it exists. When it does not, the account is not left
// as a bare "unknown": the projection says whether the plan is known to be paid, can be
// inferred as Free, or is genuinely unknown — and a Free inference gets the estimated
// window plus the usage this gateway observed inside it. The estimate is marked
// estimated / limitKnown=false, so the number is a sense of scale rather than an
// official balance, and rate-limit headers stay where they belong (throttling, never
// a subscription balance).
func projectQuota(fields map[string]interface{}, acc *store.Account, _, _ float64, observedTokens int64, usageObserved bool) {
	weekly := acc.GrokBilling.Weekly
	monthly := acc.GrokBilling.Monthly
	fields["quota_mode"] = "unknown"
	fields["quota_unit"] = "build_credits"
	fields["quota_supported"] = false
	if monthly.HasLimit {
		fields["quota_monthly_limit"] = monthly.Limit
		fields["quota_monthly_remaining"] = monthly.Remaining
		fields["quota_limit"] = monthly.Limit
		fields["quota_used"] = max(0, monthly.Limit-monthly.Remaining)
		fields["quota_remaining"] = max(0, monthly.Remaining)
		fields["quota_mode"] = "monthly"
		fields["quota_unit"] = "build_credits"
		fields["quota_supported"] = true
		provider.QuotaProvenance(fields, "paid", "upstreamBilling", "confirmed",
			"上游 Build 账单返回的月度额度", true, false)
	}
	if weekly.HasUsage {
		fields["quota_weekly_usage_percent"] = weekly.UsagePercent
		fields["quota_weekly_reset_at"] = weekly.ResetAt
		if !monthly.HasLimit {
			fields["quota_limit"] = 100.0
			fields["quota_used"] = weekly.UsagePercent
			fields["quota_remaining"] = max(0, 100-weekly.UsagePercent)
			fields["quota_mode"] = "weekly_percent"
			fields["quota_unit"] = "percent"
			fields["quota_supported"] = true
			fields["quota_reset_at"] = weekly.ResetAt
			provider.QuotaProvenance(fields, "paid", "upstreamBilling", "confirmed",
				"上游 Build 账单返回的周度窗口", true, false)
		}
	}
	// Passive response headers are a minute-scale throttle, not a subscription
	// balance, so they are published separately and never folded into quota_limit.
	if acc.GrokRateLimits.Requests.HasLimit || acc.GrokRateLimits.Requests.HasRemaining {
		fields["rate_limit_requests"] = acc.GrokRateLimits.Requests
	}
	if acc.GrokRateLimits.Tokens.HasLimit || acc.GrokRateLimits.Tokens.HasRemaining {
		fields["rate_limit_tokens"] = acc.GrokRateLimits.Tokens
	}
	if weekly.HasUsage || monthly.HasLimit {
		return
	}
	// A Free window the upstream itself reported outranks every inference: it carries
	// the account's real actual/limit pair instead of a scale reference. It only does so
	// while the window is still current — once the rolling window has passed, those
	// numbers describe a window that no longer exists, and the account falls back to the
	// estimate (still Free, because the refusal proved it) instead of showing a stale 0.
	confirmed := acc.GrokFreeQuota
	confirmedCurrent := !confirmed.ResetAt.IsZero() && time.Now().Before(confirmed.ResetAt)
	if confirmed.HasLimit && !confirmed.ConfirmedAt.IsZero() && confirmedCurrent {
		limit := confirmed.Limit
		used := confirmed.Used
		if used < 0 {
			used = 0
		}
		if used > limit {
			used = limit
		}
		fields["quota_limit"] = limit
		fields["quota_used"] = used
		fields["quota_remaining"] = max(0, limit-used)
		fields["quota_mode"] = "confirmed_free"
		fields["quota_unit"] = "tokens"
		fields["quota_supported"] = true
		fields["quota_window_hours"] = int(FreeBuildUsageWindow / time.Hour)
		if !confirmed.ResetAt.IsZero() {
			fields["quota_reset_at"] = confirmed.ResetAt
		}
		provider.QuotaProvenance(fields, "free", "upstreamExhaustion", "confirmed",
			"上游额度耗尽时返回的真实 Free 窗口（tokens actual/limit）", true, true)
		return
	}
	switch verdict := InferFreeProfile(acc); {
	case verdict.Inferred:
		limit := float64(EstimatedFreeBuildTokenLimit)
		used := float64(0)
		if usageObserved && observedTokens > 0 {
			used = float64(observedTokens)
		}
		if used > limit {
			used = limit
		}
		fields["quota_limit"] = limit
		fields["quota_used"] = used
		fields["quota_remaining"] = max(0, limit-used)
		fields["quota_mode"] = "estimated_free"
		fields["quota_unit"] = "tokens"
		fields["quota_supported"] = true
		fields["quota_window_hours"] = int(FreeBuildUsageWindow / time.Hour)
		provider.QuotaProvenance(fields, "free", verdict.Source, "estimated",
			"上游未下发数值额度；按 Free 画像估算，用量为本网关在窗口内观测到的 token", false, usageObserved)
	case BuildPlanIsPaid(acc.Subscription):
		provider.QuotaProvenance(fields, "paid", "planMetadata", "confirmed",
			"官方身份接口报告为付费套餐，但未下发数值额度窗口", false, false)
	default:
		// Never synced, or the upstream has not said anything yet. Saying anything
		// more here would be an invention.
		provider.QuotaProvenance(fields, "unknown", "unknown", "",
			"尚未同步到上游套餐或额度信息；点刷新立即同步", false, false)
	}
}
