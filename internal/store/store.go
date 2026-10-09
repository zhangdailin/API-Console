package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/modelcatalog"

	"github.com/redis/go-redis/v9"
)

var (
	ErrNoRows            = fmt.Errorf("no rows in result set")
	ErrApiKeyExpired     = fmt.Errorf("api key expired")
	ErrApiKeyRateLimited = fmt.Errorf("api key rate limit exceeded")
)

// ModelCooldownReason says what a per-model cooldown means for the accounts that
// carry it.
type ModelCooldownReason string

const (
	// ModelCooldownThrottled is the upstream asking for a pause on this model.
	// Waiting is what clears it, so the pool answers "retry later".
	ModelCooldownThrottled ModelCooldownReason = "throttled"
	// ModelCooldownUnavailable is this account's plan not covering the model. The
	// credential is healthy and no wait changes the answer, so the pool answers
	// "the model is not available on these accounts" instead of inviting a retry
	// that can only fail the same way.
	ModelCooldownUnavailable ModelCooldownReason = "unavailable"
)

// ModelCooldownEntitlementFloor is the remaining cooldown above which a stored
// deadline with no recorded reason is read as a plan verdict rather than a
// throttle. It exists for cooldowns written before reasons were stored: the two
// verdicts that create a handler-path model cooldown hold for 30 seconds and for
// a day, so anything still running after an hour cannot be the throttle.
const ModelCooldownEntitlementFloor = time.Hour

type Account struct {
	ID            int64   `json:"id"`
	Name          string  `json:"name"`
	AccountType   string  `json:"account_type"`
	ClientCookie  string  `json:"client_cookie"`
	RefreshToken  string  `json:"refresh_token,omitempty"`
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
	// it, mirroring the Grok quality guard.
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
	// ModelCooldownReasons records why each of those cooldowns exists, keyed by
	// the same model name: a throttle the upstream asked us to back off from, or
	// a plan verdict that no amount of waiting changes. The selection layer needs
	// the difference — it cannot see the request that recorded the cooldown — to
	// answer "retry later" instead of "this model is not available on these
	// accounts", and vice versa.
	//
	// It is a sibling map rather than a richer value inside ModelCooldowns on
	// purpose: the deadline map keeps the exact JSON shape earlier binaries wrote
	// and read, so rolling the gateway back cannot turn every account that
	// carries a cooldown into an undecodable document.
	ModelCooldownReasons map[string]ModelCooldownReason `json:"model_cooldown_reasons,omitempty"`

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
	// question the tier column asks. The upstream answers it at /users/me/plan:
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

type Store struct {
	accounts   accountStore
	settings   settingsStore
	apiKeys    apiKeyStore
	models     modelStore
	responses  responseStore
	reasoning  reasoningReplayStore
	keyTouchMu sync.Mutex
	keyTouches map[int64]time.Time
}

type Options struct {
	RedisAddr               string
	RedisPassword           string
	RedisDB                 int
	RedisPoolSize           int
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

// GrokCredentialPatch preserves concurrent account edits during OAuth rotation.
type GrokCredentialPatch struct {
	ExpectedRefreshToken        string
	AccessToken, RefreshToken   string
	ExpiresAt                   time.Time
	UserID, Email, Name, TeamID string
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
	// Quota carries a reading the upstream stated in band — a quota_exceeded
	// notice multiplexed into a stream — rather than one a sync round fetched.
	// It goes through the same freshness guard as a synced snapshot so a notice
	// racing a login or a manual sync cannot rewind the account's view of its
	// own allowance.
	Quota *QoderQuotaSnapshot
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
	UpdateGrokCredentials(ctx context.Context, id int64, patch GrokCredentialPatch) error
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

type modelStore interface {
	CreateModel(ctx context.Context, m *Model) error
	UpdateModel(ctx context.Context, m *Model) error
	DeleteModel(ctx context.Context, id string) error
	GetModel(ctx context.Context, id string) (*Model, error)
	ListModels(ctx context.Context) ([]*Model, error)
	GetModelByChannelAndModelID(ctx context.Context, channel, modelID string) (*Model, error)
	ReconcileDiscoveredModels(ctx context.Context, channel string, models []*Model, options ModelReconcileOptions) (*ModelReconcileResult, error)
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
	redisStore, err := newRedisStore(opts.RedisAddr, opts.RedisPassword, opts.RedisDB, opts.RedisPrefix, opts.CredentialEncryptionKey, opts.RedisPoolSize)
	if err != nil {
		return nil, fmt.Errorf("failed to init redis store: %w", err)
	}
	store.accounts = redisStore
	store.settings = redisStore
	store.apiKeys = redisStore
	store.models = redisStore
	store.responses = redisStore
	store.reasoning = redisStore
	if err := redisStore.validateAccountCredentials(context.Background()); err != nil {
		_ = redisStore.Close()
		return nil, fmt.Errorf("failed to validate account credentials: %w", err)
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

func (s *Store) UpdateGrokCredentials(ctx context.Context, id int64, patch GrokCredentialPatch) error {
	if s != nil && s.accounts != nil {
		return s.accounts.UpdateGrokCredentials(ctx, id, patch)
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
