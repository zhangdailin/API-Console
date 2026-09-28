package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/modelpolicy"
)

var (
	ErrNoRows            = fmt.Errorf("no rows in result set")
	ErrApiKeyExpired     = fmt.Errorf("api key expired")
	ErrApiKeyRateLimited = fmt.Errorf("api key rate limit exceeded")
)

type Account struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	AccountType   string  `json:"account_type"`
	SessionID     string  `json:"session_id"`
	ClientCookie  string  `json:"client_cookie"`
	RefreshToken  string  `json:"refresh_token,omitempty"`
	SessionCookie string  `json:"session_cookie"`
	ClientUat     string  `json:"client_uat"`
	UserID        string  `json:"user_id"`
	AgentMode     string  `json:"agent_mode"`
	Email         string  `json:"email"`
	Weight        int     `json:"weight"`
	MaxConcurrent int     `json:"max_concurrent,omitempty"`
	Enabled       bool    `json:"enabled"`
	Token         string  `json:"token"`        // Runtime/display token for token-backed channels
	Subscription  string  `json:"subscription"` // "free", "pro", etc.
	UsageCurrent  float64 `json:"usage_current"`
	UsageTotal    float64 `json:"usage_total"` // Used as lifetime usage
	UsageLimit    float64 `json:"usage_limit"` // Daily limit
	// TokensToday is the token spend the gateway counted for the account inside
	// the current local day, and TokensDate is the day it belongs to.
	//
	// A lifetime total alone cannot answer the question an operator actually
	// asks of an unmetered channel — "how close is this account to the upstream
	// rate limit right now?" — because a total only ever grows. The pair is
	// rolled by the counter itself: a request whose date differs from
	// TokensDate starts a new day instead of adding to yesterday's figure.
	TokensToday float64 `json:"tokens_today,omitempty"`
	TokensDate  string  `json:"tokens_date,omitempty"`
	StatusCode  string  `json:"status_code"`
	// AuthStatus is the durable credential-routing state. Empty is treated as
	// active for legacy rows; reauthRequired permanently excludes the account
	// until a successful verification or credential replacement clears it.
	AuthStatus string `json:"auth_status,omitempty"`
	// RateLimitFailures counts consecutive account-scoped 429 failures. It drives
	// the bounded exponential routing cooldown and is reset on recovery.
	RateLimitFailures int `json:"rate_limit_failures,omitempty"`
	// QualityFailures counts consecutive responses this credential returned
	// without the reasoning the request asked for (an upstream quality dump).
	// The first offence parks the credential for a cooldown; a repeat disables
	// it, mirroring grok2api's quality guard.
	QualityFailures int `json:"quality_failures,omitempty"`
	// QualityCooldownUntil parks a credential whose responses are degraded.
	QualityCooldownUntil time.Time `json:"quality_cooldown_until,omitempty"`
	// StatusMessage explains StatusCode in operator terms. A bare "401" cannot
	// distinguish "the upstream retired this grant, re-login required" from
	// "our record lost the credential", and those need different actions.
	StatusMessage string    `json:"status_message,omitempty"`
	LastAttempt   time.Time `json:"last_attempt"`
	// VerifiedAt records when the current credential last received a health
	// verdict from the upstream (any outcome). It is what lets the scheduler
	// distinguish "never checked" from "checked and healthy": LastAttempt and the
	// quota snapshot are reset by ordinary quota recovery, so they cannot tell a
	// newly added account from a verified one.
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	// ClearVerifiedAt asks the store to drop the stored verdict timestamp when a
	// credential is replaced. It exists because a zero VerifiedAt is
	// indistinguishable from "this partial update did not touch the field".
	ClearVerifiedAt bool      `json:"-"`
	QuotaResetAt    time.Time `json:"quota_reset_at"`
	RequestCount    int64     `json:"request_count"`
	LastUsedAt      time.Time `json:"last_used_at"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`

	// CredentialType is "oauth" for Grok Build CLI accounts.
	CredentialType    string    `json:"credential_type,omitempty"`
	OAuthAccessToken  string    `json:"oauth_access_token,omitempty"`
	OAuthRefreshToken string    `json:"oauth_refresh_token,omitempty"`
	OAuthExpiresAt    time.Time `json:"oauth_expires_at,omitempty"`
	TeamID            string    `json:"team_id,omitempty"`
	// GrokProvider identifies the supported xAI product boundary: Build OAuth.
	GrokProvider string `json:"grok_provider,omitempty"`
	// GrokModels is the last successful account-specific upstream /v1/models
	// capability snapshot. An empty snapshot means not synced yet, not that the
	// account supports every model.
	GrokModels         []string               `json:"grok_models,omitempty"`
	GrokModelCatalog   []modelcatalog.Profile `json:"grok_model_catalog,omitempty"`
	GrokModelsSyncedAt time.Time              `json:"grok_models_synced_at,omitempty"`
	// GrokBilling contains only official xAI Build billing information. It is
	// deliberately separate from GrokRateLimits, whose request/token headers
	// are short-lived throttling windows rather than subscription allowance.
	GrokBilling    GrokBillingSnapshot   `json:"grok_billing,omitempty"`
	GrokRateLimits GrokRateLimitSnapshot `json:"grok_rate_limits,omitempty"`
	// GrokFreeQuota stores the Free window the upstream itself reported when it
	// refused a request for having spent the included free usage. Confirmed numbers
	// take precedence over an estimated Free window.
	GrokFreeQuota GrokFreeQuotaSnapshot `json:"grok_free_quota,omitempty"`
	// ModelCooldowns records a per-model, per-account cooldown. A model the
	// upstream throttled must not take the whole account out of the pool: the
	// other models of the same account are still usable, so the verdict is scoped
	// to this map instead of StatusCode.
	ModelCooldowns map[string]time.Time `json:"model_cooldowns,omitempty"`

	// WorkBuddyAccessToken is the short-lived Keycloak bearer token of a
	// WorkBuddy (www.workbuddy.ai) account. WorkBuddyRefreshToken is the
	// durable credential and is ROTATED by Keycloak on every refresh, so the
	// rotated value must be written back. The refresh token is kept in its own
	// field instead of the generic RefreshToken slot so account responses can
	// redact it without touching other channels.
	WorkBuddyAccessToken  string    `json:"workbuddy_access_token,omitempty"`
	WorkBuddyRefreshToken string    `json:"workbuddy_refresh_token,omitempty"`
	WorkBuddyExpiresAt    time.Time `json:"workbuddy_expires_at,omitempty"`
	WorkBuddyUID          string    `json:"workbuddy_uid,omitempty"`
	// ReplaceWorkBuddyCredentials is an explicit write intent. Ordinary full
	// account updates carry a snapshot and must not overwrite a refresh token
	// that rotated after that snapshot was read.
	ReplaceWorkBuddyCredentials bool `json:"-"`
	// WorkBuddyModelIDs is the last successful account-scoped /v3/config `cli`
	// whitelist snapshot. An empty snapshot means "not synced yet", not "the
	// account supports every model".
	WorkBuddyModelIDs       []string  `json:"workbuddy_model_ids,omitempty"`
	WorkBuddyModelsSyncedAt time.Time `json:"workbuddy_models_synced_at,omitempty"`
	// WorkBuddyQuota is the last successful credit-meter snapshot. The generic
	// UsageLimit/UsageCurrent fields stay authoritative for scheduling; this
	// keeps the extra detail the meter reports (cycle reset, package label and
	// the consumption delta the account table shows).
	WorkBuddyQuota WorkBuddyQuotaSnapshot `json:"workbuddy_quota,omitempty"`

	// ── Qoder (qoder.com / openapi.qoder.sh) OAuth channel ──
	//
	// A Qoder account is created only through the official CLI device
	// authorization flow: there is no pasted personal access token. The device
	// access token is short lived, the device refresh token is the durable
	// credential and is rotated by the upstream on every refresh, so the rotated
	// value must be written back. They live in their own fields rather than the
	// generic Token/RefreshToken slots so account responses can redact them
	// without touching another channel's credential.
	QoderAccessToken        string    `json:"qoder_access_token,omitempty"`
	QoderRefreshToken       string    `json:"qoder_refresh_token,omitempty"`
	QoderExpiresAt          time.Time `json:"qoder_expires_at,omitempty"`
	ReplaceQoderCredentials bool      `json:"-"`
	// QoderMachineID is the 36-character device identity the CLI sends as
	// Cosy-MachineId / Cosy-MachineToken. It is bound to the credential: the
	// upstream rejects a request whose machine id does not match the one that
	// performed the login.
	QoderMachineID string `json:"qoder_machine_id,omitempty"`
	QoderUserID    string `json:"qoder_user_id,omitempty"`
	QoderUserName  string `json:"qoder_user_name,omitempty"`
	// QoderOrganizationID and QoderOrganizationTags come from the post-login
	// userinfo enrichment. They are optional headers: an empty value omits the
	// corresponding Cosy-Organization-* header entirely.
	QoderOrganizationID   string   `json:"qoder_organization_id,omitempty"`
	QoderOrganizationTags []string `json:"qoder_organization_tags,omitempty"`
	// QoderDataPolicy is the data-policy agreement the CLI recorded. Empty means
	// "not observed"; the client then reports `disagree`.
	QoderDataPolicy bool `json:"qoder_data_policy,omitempty"`
	// QoderRuntimeInfo and QoderRuntimeKey are the derived authentication pair
	// the gateway requires (the COSY payload's `info` and the Cosy-Key header).
	// They are derived from the credential and the identity, not supplied by the
	// operator, and they are reused across requests exactly as the CLI reuses
	// the pair it derived at login.
	QoderRuntimeInfo string `json:"qoder_runtime_info,omitempty"`
	QoderRuntimeKey  string `json:"qoder_runtime_key,omitempty"`
	// QoderModelIDs is the last successful account-scoped model catalog
	// snapshot. An empty snapshot means "not synced yet", not that the account
	// supports every model.
	QoderModelIDs       []string  `json:"qoder_model_ids,omitempty"`
	QoderModelsSyncedAt time.Time `json:"qoder_models_synced_at,omitempty"`
	// QoderQuota is the last successful credit/plan snapshot. The generic
	// UsageLimit/UsageCurrent fields stay authoritative for scheduling; this keeps
	// the extra detail the gateway reports (plan tier, the exhausted verdict and
	// the upgrade link) so the console can explain an account instead of showing
	// it as broken.
	QoderQuota QoderQuotaSnapshot `json:"qoder_quota,omitempty"`

	// ── Cline (api.cline.bot) OAuth channel ──
	//
	// A Cline account is created only through the official WorkOS device
	// authorization flow: there is no pasted personal access token. The Cline
	// access token is short lived and the Cline refresh token is the durable
	// credential, renewed at POST /auth/refresh, so the rotated value must be
	// written back. They live in their own fields rather than the generic
	// Token/RefreshToken slots so account responses can redact them without
	// touching another channel's credential.
	ClineAccessToken  string    `json:"cline_access_token,omitempty"`
	ClineRefreshToken string    `json:"cline_refresh_token,omitempty"`
	ClineExpiresAt    time.Time `json:"cline_expires_at,omitempty"`
	ClineEmail        string    `json:"cline_email,omitempty"`
	// ClinePlan is the account's subscription tier as the upstream states it.
	//
	// The recommended-models feed is per-account, but it lists four tiers at
	// once, so "the free list is non-empty" proves free access and says nothing
	// about whether the account also holds a paid plan — which is exactly the
	// question the 等级 column asks. The upstream answers it at /users/me/plan:
	// a subscriber gets a plan name, an account that never subscribed gets
	// "no plan history found for user". Empty means not probed yet, and that is
	// different from "free", so it is never defaulted.
	ClinePlan string `json:"cline_plan,omitempty"`
	// ReplaceClineCredentials is an explicit write intent. Ordinary full
	// account updates carry a snapshot and must not overwrite a refresh token
	// that rotated after that snapshot was read.
	ReplaceClineCredentials bool `json:"-"`
	// ClineModelIDs is the last successful account-scoped catalog snapshot. An
	// empty snapshot means "not synced yet", not that the account supports every
	// model.
	ClineModelIDs       []string  `json:"cline_model_ids,omitempty"`
	ClineModelsSyncedAt time.Time `json:"cline_models_synced_at,omitempty"`
}

// QoderQuotaSnapshot is one Qoder credit/plan observation.
//
// Exhausted is the gateway's own verdict and is authoritative over the
// arithmetic: an account whose counters have not refreshed can still be flagged
// spent, which is what makes it usable as a scheduling signal.
type QoderQuotaSnapshot struct {
	Limit          float64   `json:"limit,omitempty"`
	Remaining      float64   `json:"remaining,omitempty"`
	Used           float64   `json:"used,omitempty"`
	Exhausted      bool      `json:"exhausted,omitempty"`
	PlanTier       string    `json:"plan_tier,omitempty"`
	UserType       string    `json:"user_type,omitempty"`
	PaidPlan       bool      `json:"paid_plan,omitempty"`
	Unit           string    `json:"unit,omitempty"`
	UpgradeURL     string    `json:"upgrade_url,omitempty"`
	ResetAt        time.Time `json:"reset_at,omitempty"`
	PeriodEnd      time.Time `json:"period_end,omitempty"`
	LastKnownLimit float64   `json:"last_known_limit,omitempty"`
	SyncedAt       time.Time `json:"synced_at,omitempty"`
}

// ResyncAt reports when the snapshot should be refreshed again. A quota that is
// spent is the interesting case: the reset is the only moment it can recover, so
// the snapshot is worth re-reading then.
func (s QoderQuotaSnapshot) ResyncAt() time.Time {
	if s.SyncedAt.IsZero() {
		return time.Time{}
	}
	if !s.ResetAt.IsZero() {
		return s.ResetAt
	}
	return s.SyncedAt
}

// WorkBuddyQuotaSnapshot is the WorkBuddy credit-meter snapshot. Remaining/Limit
// describe the current cycle; Used/LastConsumedUnits are whole-credit figures
// derived from the meter, because the upstream also reports fractions.
type WorkBuddyQuotaSnapshot struct {
	Limit             float64   `json:"limit,omitempty"`
	Remaining         float64   `json:"remaining,omitempty"`
	Used              float64   `json:"used,omitempty"`
	PackageRemaining  float64   `json:"package_remaining,omitempty"`
	LastConsumedUnits int       `json:"last_consumed_units,omitempty"`
	ResetAt           time.Time `json:"reset_at,omitempty"`
	PeriodEnd         time.Time `json:"period_end,omitempty"`
	PackageName       string    `json:"package_name,omitempty"`
	Unit              string    `json:"unit,omitempty"`
	SyncedAt          time.Time `json:"synced_at,omitempty"`
}

// ResyncAt reports when the quota snapshot needs refreshing. The cycle reset is
// the hard deadline: the allowance is re-armed then, but the console also wants
// the displayed number to stay current between resets.
func (s WorkBuddyQuotaSnapshot) ResyncAt() time.Time {
	if s.SyncedAt.IsZero() {
		return time.Time{}
	}
	if s.ResetAt.IsZero() {
		return s.SyncedAt
	}
	return s.ResetAt
}

// GrokQuotaWindow is one explicit upstream usage or throttling dimension.
// Values are meaningful only when their Has* marker is true; zero is valid.
type GrokQuotaWindow struct {
	Limit        float64   `json:"limit,omitempty"`
	Remaining    float64   `json:"remaining,omitempty"`
	UsagePercent float64   `json:"usage_percent,omitempty"`
	HasLimit     bool      `json:"has_limit,omitempty"`
	HasRemaining bool      `json:"has_remaining,omitempty"`
	HasUsage     bool      `json:"has_usage,omitempty"`
	ResetAt      time.Time `json:"reset_at,omitempty"`
}

// GrokBillingSnapshot stores official Build weekly/monthly windows only.
type GrokBillingSnapshot struct {
	Weekly   GrokQuotaWindow `json:"weekly,omitempty"`
	Monthly  GrokQuotaWindow `json:"monthly,omitempty"`
	SyncedAt time.Time       `json:"synced_at,omitempty"`
	Source   string          `json:"source,omitempty"`
	// NextProbeAt serializes probes after an exhausted paid period ends. Before
	// the first claim PeriodEnd is the due time; each claim advances this by the
	// bounded retry interval so concurrent selectors cannot hammer billing.
	NextProbeAt time.Time `json:"next_probe_at,omitempty"`
	LastProbeAt time.Time `json:"last_probe_at,omitempty"`
}

const GrokPaidQuotaProbeInterval = 15 * time.Minute

// IsExhausted reports an authoritative paid-billing exhaustion signal. Monthly
// numeric allowance wins when present; otherwise a 100% weekly usage snapshot
// is sufficient when it also carries a real billing period.
func (b GrokBillingSnapshot) IsExhausted() bool {
	if b.Monthly.HasLimit && b.Monthly.Limit > 0 && b.Monthly.HasRemaining && b.Monthly.Remaining <= 0 {
		return true
	}
	return b.Weekly.HasUsage && b.Weekly.UsagePercent >= 100 && !b.Weekly.ResetAt.IsZero()
}

// PeriodEnd returns the latest known paid billing reset.
func (b GrokBillingSnapshot) PeriodEnd() time.Time {
	if b.Monthly.ResetAt.After(b.Weekly.ResetAt) {
		return b.Monthly.ResetAt
	}
	return b.Weekly.ResetAt
}

// GrokRateLimitSnapshot stores passive response headers separately from
// billing. They can be useful for cooldown and diagnostics but must never be
// rendered as a paid-plan balance.
type GrokRateLimitSnapshot struct {
	Requests   GrokQuotaWindow `json:"requests,omitempty"`
	Tokens     GrokQuotaWindow `json:"tokens,omitempty"`
	Model      string          `json:"model,omitempty"`
	ObservedAt time.Time       `json:"observed_at,omitempty"`
}

// GrokFreeQuotaSnapshot is the Free allowance window the upstream CONFIRMED by
// refusing a request ("subscription:free-usage-exhausted ... tokens (actual/limit):
// N/M"). It is the one place a Free limit becomes a fact rather than an estimate, so
// it is kept apart from GrokBilling (a paid window this account never returned) and
// from the estimate derived from an inferred Free profile.
type GrokFreeQuotaSnapshot struct {
	Used        float64   `json:"used,omitempty"`
	Limit       float64   `json:"limit,omitempty"`
	HasLimit    bool      `json:"has_limit,omitempty"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	ConfirmedAt time.Time `json:"confirmed_at,omitempty"`
}

// Qoder quota exhaustion is a capability downgrade when, and only when, the
// requested route is explicitly marked free by the current upstream catalog.
// The selector keeps these accounts in the pool but its model filter rejects
// every metered or unknown route.
const (
	AccountStatusQoderQuotaExhausted     = "qoder_quota_exhausted"
	AccountStatusWorkBuddyQuotaExhausted = "workbuddy_quota_exhausted"
)

const (
	AccountAuthStatusActive         = "active"
	AccountAuthStatusReauthRequired = "reauthRequired"
)

// AccountAuthActive preserves compatibility with rows created before AuthStatus
// existed while making every non-active explicit state ineligible for routing.
func AccountAuthActive(acc *Account) bool {
	if acc == nil {
		return false
	}
	status := strings.TrimSpace(acc.AuthStatus)
	return status == "" || strings.EqualFold(status, AccountAuthStatusActive)
}

type ApiKey struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	KeyHash       string   `json:"-"`
	KeyFull       string   `json:"-"`
	KeyPrefix     string   `json:"key_prefix"`
	KeySuffix     string   `json:"key_suffix"`
	Enabled       bool     `json:"enabled"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	RPMLimit      int      `json:"rpm_limit,omitempty"`
	MaxConcurrent int      `json:"max_concurrent,omitempty"`
	// BillingLimitUSDTicks caps how much this key may spend, in USD ticks
	// (1 USD = 10,000,000,000 ticks). Zero means unlimited, so a key created
	// before this field existed keeps working unchanged.
	BillingLimitUSDTicks int64 `json:"billing_limit_usd_ticks,omitempty"`
	// BillingUsedUSDTicks is a read-only projection of the settled usage
	// counter. The Redis counter is authoritative; this field reports it.
	BillingUsedUSDTicks int64 `json:"billing_used_usd_ticks,omitempty"`
	// BillingPeriodDays rolls the settled usage over on a fixed period, the way
	// grok2api resets a key at the end of its billing period. Zero means the
	// counter only ever moves when an operator resets it.
	BillingPeriodDays int `json:"billing_period_days,omitempty"`
	// BillingPeriodStartedAt is when the current period began. It is written by
	// the rollover, not by the caller.
	BillingPeriodStartedAt time.Time  `json:"billing_period_started_at,omitempty"`
	ExpiresAt              *time.Time `json:"expires_at,omitempty"`
	LastUsedAt             *time.Time `json:"last_used_at"`
	CreatedAt              time.Time  `json:"created_at"`
}

// StoredResponse records the ownership needed to continue or manage an
// upstream Responses resource without retaining the request or response body.
type StoredResponse struct {
	ResponseID     string `json:"response_id"`
	OwnerHash      string `json:"owner_hash"`
	AccountID      int64  `json:"account_id"`
	Model          string `json:"model"`
	Provider       string `json:"provider"`
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	ContentType    string `json:"content_type,omitempty"`
	Body           []byte `json:"body,omitempty"`
	// InputItems is the request input the response was created from, normalized
	// to the Responses item shape. GET /responses/{id}/input_items serves it
	// back. Records written before this field existed simply report an empty
	// list rather than failing, and previous_response_id expansion is unaffected
	// because it reads Body.
	InputItems json.RawMessage `json:"input_items,omitempty"`
	// PreviousResponseID links a continuation to the response it continued, so
	// the stored input list can report the whole conversation instead of only
	// the last turn.
	PreviousResponseID string    `json:"previous_response_id,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// StoredReasoningReplay contains one opaque encrypted reasoning item. The key
// is already tenant/model/session isolated by the gateway; Redis persistence
// lets later turns resume on another replica without storing plaintext chain
// of thought.
type StoredReasoningReplay struct {
	Model      string `json:"model"`
	SessionKey string `json:"session_key"`
	// EncryptedContent is the legacy single-cipher form. It is still written by
	// paths that only observe one opaque reasoning item, and is always read for
	// compatibility; Items takes precedence when present.
	EncryptedContent string            `json:"encrypted_content,omitempty"`
	Items            []json.RawMessage `json:"items,omitempty"`
	ExpiresAt        time.Time         `json:"expires_at"`
}

type StoredSessionAffinity struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	SessionKey string    `json:"session_key"`
	AccountID  int64     `json:"account_id"`
	ExpiresAt  time.Time `json:"expires_at"`
}

type Store struct {
	accounts  accountStore
	settings  settingsStore
	apiKeys   apiKeyStore
	models    modelStore
	responses responseStore
	reasoning reasoningReplayStore
}

type Options struct {
	StoreMode               string
	RedisAddr               string
	RedisPassword           string
	RedisDB                 int
	RedisPrefix             string
	CredentialEncryptionKey []byte
}

// WorkBuddyCredentialPatch contains only the fields owned by a WorkBuddy token
// refresh. Keeping this mutation narrow prevents a client built from an older
// account snapshot from overwriting concurrent quota, status or admin edits.
type WorkBuddyCredentialPatch struct {
	ExpectedRefreshToken string
	AccessToken          string
	RefreshToken         string
	ExpiresAt            time.Time
	UID                  string
	Email                string
}

// QoderAccountPatch contains the independently refreshed Qoder client state.
// Nil slices mean "not changed"; the remaining zero values keep the stored
// value, matching the provider's rotated-credential semantics.
type QoderAccountPatch struct {
	ExpectedRefreshToken string
	AccessToken          string
	RefreshToken         string
	ExpiresAt            time.Time
	UserID               string
	RuntimeInfo          string
	RuntimeKey           string
	ModelIDs             []string
}

// ClineCredentialPatch contains the independently refreshed Cline client state.
// Nil slices mean "not changed"; the remaining zero values keep the stored
// value, matching the provider's rotated-credential semantics.
type ClineCredentialPatch struct {
	ExpectedRefreshToken string
	AccessToken          string
	RefreshToken         string
	ExpiresAt            time.Time
	Email                string
	ModelIDs             []string
}

type accountStore interface {
	CreateAccount(ctx context.Context, acc *Account) error
	UpdateAccount(ctx context.Context, acc *Account) error
	UpdateAccountQuality(ctx context.Context, id int64, failures int, cooldownUntil time.Time) error
	UpdateWorkBuddyCredentials(ctx context.Context, id int64, patch WorkBuddyCredentialPatch) error
	UpdateQoderAccount(ctx context.Context, id int64, patch QoderAccountPatch) error
	UpdateClineCredentials(ctx context.Context, id int64, patch ClineCredentialPatch) error
	DeleteAccount(ctx context.Context, id int64) error
	GetAccount(ctx context.Context, id int64) (*Account, error)
	ListAccounts(ctx context.Context) ([]*Account, error)
	GetEnabledAccounts(ctx context.Context) ([]*Account, error)
	IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error
	IncrementAccountStatsOperation(ctx context.Context, id int64, usage float64, count int64, operationID string, completedAt time.Time) error
	ConsumeGrokQuota(ctx context.Context, id int64, provider string, amount float64) (bool, error)
	ClaimGrokPaidQuotaProbe(ctx context.Context, id int64, now time.Time) (bool, error)
}

type settingsStore interface {
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
}

type apiKeyStore interface {
	CreateApiKey(ctx context.Context, key *ApiKey) error
	ListApiKeys(ctx context.Context) ([]*ApiKey, error)
	UpdateApiKey(ctx context.Context, key *ApiKey) error
	DeleteApiKey(ctx context.Context, id int64) error
	GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error)
	GetApiKeyByHash(ctx context.Context, hash string) (*ApiKey, error)
	ConsumeApiKeyRPM(ctx context.Context, id int64, limit int, now time.Time) (bool, error)
	TouchApiKeyLastUsed(ctx context.Context, id int64, now time.Time) error
	ReserveApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64, expiresAt time.Time) (bool, error)
	SettleApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64) error
	ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error)
	ResetApiKeyBilling(ctx context.Context, id int64) error
	RolloverApiKeyBilling(ctx context.Context, key *ApiKey, now time.Time) (bool, error)
}

type modelStore interface {
	CreateModel(ctx context.Context, m *Model) error
	UpdateModel(ctx context.Context, m *Model) error
	DeleteModel(ctx context.Context, id string) error
	GetModel(ctx context.Context, id string) (*Model, error)
	ListModels(ctx context.Context) ([]*Model, error)
	GetModelByModelID(ctx context.Context, modelID string) (*Model, error)
	GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error)
	ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error)
}

type responseStore interface {
	SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error
	GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error)
	DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error
}

type reasoningReplayStore interface {
	DeleteReasoningReplay(ctx context.Context, model, sessionKey string) error
	SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error
	GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error)
	SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error
	GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error)
}

// SetChangeEmitter wires account-change notifications. Passing nil disables
// them, which keeps a store used only by tests silent.
func (s *Store) SetChangeEmitter(emitter ChangeEmitter) {
	if s == nil {
		return
	}
	if redis, ok := s.accounts.(*redisStore); ok {
		redis.SetChangeEmitter(emitter)
	}
}

func New(opts Options) (*Store, error) {
	store := &Store{}
	redisStore, err := newRedisStore(opts.RedisAddr, opts.RedisPassword, opts.RedisDB, opts.RedisPrefix, opts.CredentialEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to init redis store: %w", err)
	}
	store.accounts = redisStore
	store.settings = redisStore
	store.apiKeys = redisStore
	store.models = redisStore
	store.responses = redisStore
	store.reasoning = redisStore
	if err := redisStore.migrateLegacyAccountCredentials(context.Background()); err != nil {
		_ = redisStore.Close()
		return nil, fmt.Errorf("failed to migrate account credentials: %w", err)
	}
	store.prepareModels()
	return store, nil
}

// prepareModels performs the startup maintenance model management needs.
//
// It deliberately creates no model rows. Every published model is an
// observation of an upstream catalog made by a refresh with an active account,
// so a fresh deployment starts with an empty catalog and fills it from
// upstream. Seeding a compiled-in list here would make model management report
// models that no account ever advertised, and would keep them served after the
// upstream withdrew them.
func (s *Store) prepareModels() {
	ctx := context.Background()
	// Deprecated identifiers are removed because they are known-dead names that
	// must not stay routable; this inspects stored rows and never adds any.
	s.cleanupDeprecatedModelIDs(ctx)
	// Route metadata for stored Grok rows is repaired in place.
	s.backfillGrokRouteMetadata(ctx)
}

func (s *Store) backfillGrokRouteMetadata(ctx context.Context) {
	models, err := s.ListModels(ctx)
	if err != nil {
		return
	}
	for _, model := range models {
		if model == nil || !strings.EqualFold(strings.TrimSpace(model.Channel), "grok") {
			continue
		}
		if model.Provider != "" && model.UpstreamModel != "" && len(model.Capabilities) > 0 {
			continue
		}
		updated := *model
		applyGrokRouteDefaults(&updated)
		if err := s.UpdateModel(ctx, &updated); err != nil {
			slog.Warn("failed to backfill Grok Build route metadata", "model_id", model.ModelID, "error", err)
		}
	}
}

// deprecatedModelIDsByChannel documents the rule below: a retired identifier is
// retired *within a channel's namespace*, not everywhere.
//
// The list used to be applied by identifier alone. That deleted working models:
// a channel catalog may legitimately advertise grok-4.3, grok-4.20-* and
// grok-build-0.1 (they route xAI models), so every restart removed rows a
// refresh had just published, and a refresh put them back. The channel is
// therefore part of the entry.
//
// The Grok entries are the runtime interception list in modelpolicy
// (deprecatedGrokModelIDs) plus the Grok-channel extra "grok-4.3"; keep the two
// in step by deriving from modelpolicy rather than editing both lists.
var deprecatedModelIDsByChannel = func() map[string][]string {
	grokIDs := make([]string, 0, len(modelpolicy.DeprecatedGrokModelIDs)+1)
	for id := range modelpolicy.DeprecatedGrokModelIDs {
		grokIDs = append(grokIDs, id)
	}
	// grok-4.3 is deprecated for the Grok channel only: other channels may still
	// route it.
	grokIDs = append(grokIDs, "grok-4.3")
	return map[string][]string{
		"Grok": grokIDs,
	}
}()

// cleanupDeprecatedModelIDs removes retired identifiers from the channel whose
// namespace retired them. It inspects stored rows and never adds any.
func (s *Store) cleanupDeprecatedModelIDs(ctx context.Context) {
	for channel, modelIDs := range deprecatedModelIDsByChannel {
		for _, modelID := range modelIDs {
			m, err := s.GetModelByChannelAndModelID(ctx, channel, modelID)
			if err != nil || m == nil {
				continue
			}
			if err := s.DeleteModel(ctx, m.ID); err != nil {
				slog.Warn("Failed to remove deprecated model", "channel", channel, "model_id", modelID, "error", err)
				continue
			}
			slog.Debug("Removed deprecated model", "channel", channel, "model_id", modelID)
		}
	}
}

func applyGrokRouteDefaults(model *Model) {
	if model == nil {
		return
	}
	id := strings.ToLower(strings.TrimSpace(model.ModelID))
	model.Origin = "catalog"
	model.Provider = "build"
	model.UpstreamModel = strings.TrimPrefix(id, "build/")
	model.Capabilities = []string{CapabilityChat, CapabilityMessages, CapabilityResponses}
}

// ApplyGrokRouteDefaults initializes route metadata for catalog/discovery
// records while allowing callers to override Origin afterwards.
func ApplyGrokRouteDefaults(model *Model) { applyGrokRouteDefaults(model) }

func (s *Store) Close() error {
	if rs, ok := s.accounts.(*redisStore); ok {
		return rs.Close()
	}
	return nil
}

// RedisClient returns the underlying Redis client, or nil if not using Redis.
func (s *Store) RedisClient() *redis.Client {
	if rs, ok := s.accounts.(*redisStore); ok {
		return rs.Client()
	}
	return nil
}

// RedisPrefix returns the configured key prefix.
func (s *Store) RedisPrefix() string {
	if s.accounts != nil {
		if rs, ok := s.accounts.(*redisStore); ok {
			return rs.prefix
		}
	}
	return "orchids:"
}

func (s *Store) CreateAccount(ctx context.Context, acc *Account) error {
	if s.accounts != nil {
		return s.accounts.CreateAccount(ctx, acc)
	}
	return fmt.Errorf("store not configured")
}

// UpdateAccountQuality records a quality verdict (failures + park window) for one
// account without rewriting the rest of it.
func (s *Store) UpdateAccountQuality(ctx context.Context, id int64, failures int, cooldownUntil time.Time) error {
	if s == nil || s.accounts == nil {
		return fmt.Errorf("account store not configured")
	}
	return s.accounts.UpdateAccountQuality(ctx, id, failures, cooldownUntil)
}

func (s *Store) UpdateAccount(ctx context.Context, acc *Account) error {
	if s.accounts != nil {
		return s.accounts.UpdateAccount(ctx, acc)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) UpdateWorkBuddyCredentials(ctx context.Context, id int64, patch WorkBuddyCredentialPatch) error {
	if s.accounts != nil {
		return s.accounts.UpdateWorkBuddyCredentials(ctx, id, patch)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) UpdateQoderAccount(ctx context.Context, id int64, patch QoderAccountPatch) error {
	if s.accounts != nil {
		return s.accounts.UpdateQoderAccount(ctx, id, patch)
	}
	return fmt.Errorf("store not configured")
}

// UpdateClineCredentials persists a rotated Cline credential atomically.
func (s *Store) UpdateClineCredentials(ctx context.Context, id int64, patch ClineCredentialPatch) error {
	if s.accounts != nil {
		return s.accounts.UpdateClineCredentials(ctx, id, patch)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	if s.accounts != nil {
		return s.accounts.DeleteAccount(ctx, id)
	}
	return fmt.Errorf("store not configured")
}

func (s *Store) GetAccount(ctx context.Context, id int64) (*Account, error) {
	if s.accounts != nil {
		return s.accounts.GetAccount(ctx, id)
	}
	return nil, fmt.Errorf("store not configured")
}

func (s *Store) ListAccounts(ctx context.Context) ([]*Account, error) {
	if s.accounts != nil {
		return s.accounts.ListAccounts(ctx)
	}
	return nil, fmt.Errorf("store not configured")
}

func (s *Store) GetEnabledAccounts(ctx context.Context) ([]*Account, error) {
	if s.accounts != nil {
		return s.accounts.GetEnabledAccounts(ctx)
	}
	return nil, fmt.Errorf("store not configured")
}

func (s *Store) IncrementAccountStats(ctx context.Context, id int64, usage float64, count int64) error {
	return s.IncrementAccountStatsOperation(ctx, id, usage, count, "", time.Now().UTC())
}

// IncrementAccountStatsOperation applies one completed request's counters. A
// non-empty operationID makes retries durable and idempotent across processes;
// completedAt determines the fixed UTC daily bucket rather than retry time.
func (s *Store) IncrementAccountStatsOperation(ctx context.Context, id int64, usage float64, count int64, operationID string, completedAt time.Time) error {
	if s.accounts != nil {
		return s.accounts.IncrementAccountStatsOperation(ctx, id, usage, count, operationID, completedAt)
	}
	return fmt.Errorf("store not configured")
}

// ConsumeGrokQuota atomically applies successful request units to an observed
// local quota snapshot. It returns false when the provider has no compatible
// request-unit window, deliberately leaving weekly percentage billing alone.
func (s *Store) ConsumeGrokQuota(ctx context.Context, id int64, provider string, amount float64) (bool, error) {
	if s.accounts != nil {
		return s.accounts.ConsumeGrokQuota(ctx, id, provider, amount)
	}
	return false, fmt.Errorf("store not configured")
}

// ClaimGrokPaidQuotaProbe atomically admits at most one paid-billing probe per
// interval once the known billing period has ended.
func (s *Store) ClaimGrokPaidQuotaProbe(ctx context.Context, id int64, now time.Time) (bool, error) {
	if s.accounts != nil {
		return s.accounts.ClaimGrokPaidQuotaProbe(ctx, id, now)
	}
	return false, fmt.Errorf("store not configured")
}

func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	if s.settings != nil {
		return s.settings.GetSetting(ctx, key)
	}
	return "", fmt.Errorf("settings store not configured")
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if s.settings != nil {
		return s.settings.SetSetting(ctx, key, value)
	}
	return fmt.Errorf("settings store not configured")
}

func (s *Store) CreateApiKey(ctx context.Context, key *ApiKey) error {
	if s.apiKeys != nil {
		return s.apiKeys.CreateApiKey(ctx, key)
	}
	return fmt.Errorf("api keys store not configured")
}

// rolloverApiKeyBilling atomically advances an elapsed billing period in the
// durable store. The Redis transaction updates the period record and resets only
// settled usage/idempotency state; live holds remain attached to running requests.
func (s *Store) rolloverApiKeyBilling(ctx context.Context, key *ApiKey, now time.Time) {
	if key == nil || key.BillingPeriodDays <= 0 {
		return
	}
	rolled, err := s.apiKeys.RolloverApiKeyBilling(ctx, key, now.UTC())
	if err != nil {
		slog.Warn("failed to roll the billing period over", "key_id", key.ID, "error", err)
		return
	}
	if rolled {
		key.BillingUsedUSDTicks = 0
		key.BillingPeriodStartedAt = now.UTC()
	}
}

// AuthorizeApiKey authenticates a raw client key and atomically applies its
// optional per-minute request limit. Raw keys are never persisted by this path.
func (s *Store) AuthorizeApiKey(ctx context.Context, raw string) (*ApiKey, error) {
	if s == nil || s.apiKeys == nil {
		return nil, fmt.Errorf("api key store not configured")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrNoRows
	}
	digest := sha256.Sum256([]byte(raw))
	key, err := s.apiKeys.GetApiKeyByHash(ctx, hex.EncodeToString(digest[:]))
	if err != nil {
		return nil, err
	}
	if key == nil || !key.Enabled {
		return nil, ErrNoRows
	}
	now := time.Now().UTC()
	if key.ExpiresAt != nil && !now.Before(key.ExpiresAt.UTC()) {
		return nil, ErrApiKeyExpired
	}
	// A key without an explicit per-minute limit has none. A silent 60 RPM
	// default throttled a caller that never asked for a limit; the deployment's
	// admission control is what protects the gateway.
	if rpm := key.RPMLimit; rpm > 0 {
		allowed, err := s.apiKeys.ConsumeApiKeyRPM(ctx, key.ID, rpm, now)
		if err != nil {
			return nil, err
		}
		if !allowed {
			return nil, ErrApiKeyRateLimited
		}
	}
	key.LastUsedAt = &now
	if key.RPMLimit <= 0 {
		// Persist only the usage touch. Rewriting the stale row here could race an
		// atomic billing rollover and restore its old period start.
		if err := s.apiKeys.TouchApiKeyLastUsed(ctx, key.ID, now); err != nil {
			return nil, err
		}
	}
	s.rolloverApiKeyBilling(ctx, key, now)
	return key, nil
}

func (s *Store) ListApiKeys(ctx context.Context) ([]*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.ListApiKeys(ctx)
	}
	return nil, fmt.Errorf("api keys store not configured")
}

func (s *Store) UpdateApiKey(ctx context.Context, key *ApiKey) error {
	if s != nil && s.apiKeys != nil {
		return s.apiKeys.UpdateApiKey(ctx, key)
	}
	return fmt.Errorf("api keys store not configured")
}

func (s *Store) DeleteApiKey(ctx context.Context, id int64) error {
	if s.apiKeys != nil {
		return s.apiKeys.DeleteApiKey(ctx, id)
	}
	return fmt.Errorf("api keys store not configured")
}

func (s *Store) GetApiKeyByID(ctx context.Context, id int64) (*ApiKey, error) {
	if s.apiKeys != nil {
		return s.apiKeys.GetApiKeyByID(ctx, id)
	}
	return nil, fmt.Errorf("api keys store not configured")
}

// ReserveApiKeyBilling atomically holds amount ticks of a key's billing limit
// for one in-flight request. It returns false (with no error) when the limit
// does not cover the request, and true when the hold already existed for the
// same event id — retrying a request must not double reserve.
func (s *Store) ReserveApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64, expiresAt time.Time) (bool, error) {
	if s == nil || s.apiKeys == nil {
		return false, fmt.Errorf("api key store not configured")
	}
	return s.apiKeys.ReserveApiKeyBilling(ctx, id, eventID, amount, expiresAt)
}

// SettleApiKeyBilling converts a hold into settled usage: the reservation is
// dropped and amount ticks are added to the key's used counter. Actual usage is
// authoritative, so settling an event whose hold already expired still charges.
func (s *Store) SettleApiKeyBilling(ctx context.Context, id int64, eventID string, amount int64) error {
	if s == nil || s.apiKeys == nil {
		return fmt.Errorf("api key store not configured")
	}
	return s.apiKeys.SettleApiKeyBilling(ctx, id, eventID, amount)
}

// ReleaseApiKeyBilling drops a hold without charging it and reports whether one
// was actually held, so a settling path cannot be charged twice.
func (s *Store) ReleaseApiKeyBilling(ctx context.Context, id int64, eventID string) (bool, error) {
	if s == nil || s.apiKeys == nil {
		return false, fmt.Errorf("api key store not configured")
	}
	return s.apiKeys.ReleaseApiKeyBilling(ctx, id, eventID)
}

// ResetApiKeyBilling zeroes the settled usage counter and drops every
// outstanding reservation for the key. The configured limit is left in place.
func (s *Store) ResetApiKeyBilling(ctx context.Context, id int64) error {
	if s == nil || s.apiKeys == nil {
		return fmt.Errorf("api key store not configured")
	}
	return s.apiKeys.ResetApiKeyBilling(ctx, id)
}

func (s *Store) SaveStoredResponse(ctx context.Context, response *StoredResponse, ttl time.Duration) error {
	if s == nil || s.responses == nil {
		return fmt.Errorf("response store not configured")
	}
	return s.responses.SaveStoredResponse(ctx, response, ttl)
}

func (s *Store) GetStoredResponse(ctx context.Context, responseID, ownerHash string) (*StoredResponse, error) {
	if s == nil || s.responses == nil {
		return nil, fmt.Errorf("response store not configured")
	}
	return s.responses.GetStoredResponse(ctx, responseID, ownerHash)
}

func (s *Store) DeleteStoredResponse(ctx context.Context, responseID, ownerHash string) error {
	if s == nil || s.responses == nil {
		return fmt.Errorf("response store not configured")
	}
	return s.responses.DeleteStoredResponse(ctx, responseID, ownerHash)
}

func (s *Store) DeleteReasoningReplay(ctx context.Context, model, key string) error {
	if s == nil || s.reasoning == nil {
		return nil
	}
	return s.reasoning.DeleteReasoningReplay(ctx, model, key)
}

func (s *Store) SaveReasoningReplay(ctx context.Context, replay *StoredReasoningReplay, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.SaveReasoningReplay(ctx, replay, ttl)
}

func (s *Store) GetReasoningReplay(ctx context.Context, model, sessionKey string) (*StoredReasoningReplay, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("reasoning replay store not configured")
	}
	return s.reasoning.GetReasoningReplay(ctx, model, sessionKey)
}

func (s *Store) SaveSessionAffinity(ctx context.Context, affinity *StoredSessionAffinity, ttl time.Duration) error {
	if s == nil || s.reasoning == nil {
		return fmt.Errorf("session affinity store not configured")
	}
	return s.reasoning.SaveSessionAffinity(ctx, affinity, ttl)
}

func (s *Store) GetSessionAffinity(ctx context.Context, provider, model, sessionKey string) (*StoredSessionAffinity, error) {
	if s == nil || s.reasoning == nil {
		return nil, fmt.Errorf("session affinity store not configured")
	}
	return s.reasoning.GetSessionAffinity(ctx, provider, model, sessionKey)
}

// Model wrappers

func (s *Store) CreateModel(ctx context.Context, m *Model) error {
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	s.clearOtherModelDefaults(ctx, m, false)
	return s.models.CreateModel(ctx, m)
}

func (s *Store) UpdateModel(ctx context.Context, m *Model) error {
	if m != nil {
		m.NormalizeRoute()
	}
	if s.models == nil {
		return fmt.Errorf("models store not configured")
	}
	s.clearOtherModelDefaults(ctx, m, true)
	return s.models.UpdateModel(ctx, m)
}

func (s *Store) clearOtherModelDefaults(ctx context.Context, m *Model, excludeSelf bool) {
	if !m.IsDefault {
		return
	}
	models, err := s.models.ListModels(ctx)
	if err != nil {
		return
	}
	for _, other := range models {
		if other.Channel != m.Channel || excludeSelf && other.ID == m.ID || !other.IsDefault {
			continue
		}
		other.IsDefault = false
		if err := s.models.UpdateModel(ctx, other); err != nil {
			slog.Warn("Failed to clear default flag on model", "model_id", other.ModelID, "error", err)
		}
	}
}

func (s *Store) DeleteModel(ctx context.Context, id string) error {
	if s.models != nil {
		return s.models.DeleteModel(ctx, id)
	}
	return fmt.Errorf("models store not configured")
}

func (s *Store) GetModel(ctx context.Context, id string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModel(ctx, id)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) GetModelByModelID(ctx context.Context, modelID string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModelByModelID(ctx, modelID)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error) {
	if s.models != nil {
		return s.models.GetModelByChannelAndModelID(ctx, channel, modelID)
	}
	return nil, fmt.Errorf("models store not configured")
}

func (s *Store) ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error) {
	if s == nil || s.models == nil {
		return nil, fmt.Errorf("models store not configured")
	}
	return s.models.ReconcileDiscoveredModels(ctx, channel, models, options)
}

func (s *Store) ListModels(ctx context.Context) ([]*Model, error) {
	if s.models != nil {
		return s.models.ListModels(ctx)
	}
	return nil, fmt.Errorf("models store not configured")
}

// Secrets returns every credential-bearing value the account holds.
//
// It lives beside the type because the type is what grows new credential fields,
// and it is the one list every redactor must use. There were three hand-maintained
// copies: the account response redactor, the attempt-diagnostics scrubber and this
// one. They had already drifted — diagnostics redacted seven of the fourteen values
// — and each copy is a place where a newly added credential field is silently
// published.
//
// Callers that redact free text should replace each returned value, quoted or not:
// upstream errors echo credentials back in both forms.
func (a *Account) Secrets() []string {
	if a == nil {
		return nil
	}
	return []string{
		a.Token,
		a.ClientCookie,
		a.RefreshToken,
		a.SessionCookie,
		a.SessionID,
		a.ClientUat,
		a.OAuthAccessToken,
		a.OAuthRefreshToken,
		a.WorkBuddyAccessToken,
		a.WorkBuddyRefreshToken,
		a.QoderAccessToken,
		a.QoderRefreshToken,
		a.QoderRuntimeInfo,
		a.QoderRuntimeKey,
		a.ClineAccessToken,
		a.ClineRefreshToken,
	}
}
