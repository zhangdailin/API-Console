package web

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apperrors "orchids-api/internal/errors"
)

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

const grokModelValidationCacheTTL = 3 * time.Second

// Handler serves the Grok Web (grok.com SSO) plane: AppChat conversations,
// Imagine images, videos, media inputs, the media cache and the SSO account
// tools. The parent grok channel dispatches Web models here.
type Handler struct {
	base          *baseHandler
	runtimeMu     sync.RWMutex
	cfg           *config.Config
	lb            *loadbalancer.LoadBalancer
	client        *Client
	connTracker   loadbalancer.ConnTracker
	modelCacheMu  sync.RWMutex
	modelCache    map[string]time.Time
	sessionMu     sync.Mutex
	affinityLocks [64]sync.Mutex
	affinity      map[string]sessionAffinityEntry
	instanceID    string
	auditLogger   audit.Logger

	quotaOnce    sync.Once
	quotaMu      sync.Mutex
	quotaPending map[int64]grokQuotaSyncJob
	quotaWake    chan struct{}
}

// chatOutcome is what one Web turn produced, for the audit journal.
type chatOutcome struct {
	Usage       map[string]interface{}
	UsageSource audit.UsageSource
	Finish      string
	FirstToken  time.Time
	Err         error
}

type chatAccountSession struct {
	acc            *store.Account
	token          string
	poolCandidates []string
	release        func()
}

type imageEditUploadInput struct {
	mime string
	data []byte
}

type imageEditReference struct {
	fileID string
}

func NewHandler(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	if lb != nil && lb.Store != nil {
		configureDistributedGrokLimits(lb.Store.RedisClient(), lb.Store.RedisPrefix())
	}
	instanceID := "grok-web-" + randomHex(16)
	if cfg != nil && strings.TrimSpace(cfg.DeploymentInstance) != "" {
		instanceID = strings.TrimSpace(cfg.DeploymentInstance)
	}
	if cfg != nil && strings.TrimSpace(cfg.MediaDir) != "" {
		cacheBaseDir = strings.TrimSpace(cfg.MediaDir)
	}
	h := &Handler{
		base:        newBaseHandler(lb),
		cfg:         cfg,
		lb:          lb,
		client:      New(cfg),
		connTracker: loadbalancer.NewMemoryConnTracker(),
		modelCache:  make(map[string]time.Time),
		affinity:    make(map[string]sessionAffinityEntry),
		instanceID:  instanceID,
		auditLogger: audit.NewNopLogger(),
	}
	return h
}

// SetConfig replaces the immutable config and the client whose transport is
// derived from it. Existing requests retain their client pointers; new requests
// immediately observe the new proxy, timeout and endpoint settings.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil || cfg == nil {
		return
	}
	client := New(cfg)
	h.runtimeMu.Lock()
	previous := h.cfg
	h.cfg = cfg
	h.client = client
	h.runtimeMu.Unlock()
	if webTransportKey(previous) != webTransportKey(cfg) {
		// A new proxy or clearance can make grok.com reachable: retry the Web
		// accounts that have not synced instead of waiting out their backoff.
		maintenanceAttempts.Clear()
		go h.resyncUnsynced()
	}
}

// resyncUnsynced re-reads the Web accounts without a current observation.
func (h *Handler) resyncUnsynced() {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	accounts, err := h.lb.Store.ListAccounts(ctx)
	if err != nil {
		return
	}
	pending := make([]*store.Account, 0, len(accounts))
	for _, acc := range accounts {
		if IsWebAccount(acc) && acc.Enabled && store.AccountAuthActive(acc) && !accountObservesRoutes(acc, time.Now()) {
			pending = append(pending, acc)
		}
	}
	if len(pending) > 0 {
		h.refreshAccounts(ctx, pending)
	}
}

func (h *Handler) configSnapshot() *config.Config {
	if h == nil {
		return nil
	}
	return util.ReadSnapshot(&h.runtimeMu, &h.cfg)
}

func (h *Handler) webClient() *Client {
	if h == nil {
		return nil
	}
	return util.ReadSnapshot(&h.runtimeMu, &h.client)
}

func (h *Handler) SetAuditLogger(logger audit.Logger) {
	if h != nil && logger != nil {
		h.runtimeMu.Lock()
		h.auditLogger = logger
		h.runtimeMu.Unlock()
	}
}

func (h *Handler) auditLoggerSnapshot() audit.Logger {
	if h == nil {
		return nil
	}
	return util.ReadSnapshot(&h.runtimeMu, &h.auditLogger)
}

func (h *Handler) auditChatOutcome(ctx context.Context, acc *store.Account, req *ChatCompletionsRequest, result chatOutcome) {
	logger := h.auditLoggerSnapshot()
	if logger == nil {
		return
	}
	status, message := result.Finish, ""
	if result.Err != nil {
		status = "error"
		message = result.Err.Error()
	}
	usage := result.Usage
	prompt, _ := usage["prompt_tokens_details"].(map[string]interface{})
	completion, _ := usage["completion_tokens_details"].(map[string]interface{})
	metadata := map[string]interface{}{"finish_reason": result.Finish}
	addReasoningDiagnostics(ctx, metadata)
	duration := int64(0)
	if !req.startedAt.IsZero() {
		duration = time.Since(req.startedAt).Milliseconds()
		if !result.FirstToken.IsZero() {
			metadata["first_token_ms"] = result.FirstToken.Sub(req.startedAt).Milliseconds()
		}
	}
	accountID := int64(0)
	if acc != nil {
		accountID = acc.ID
	}
	usageSource := result.UsageSource
	if usageSource == "" {
		if result.Err != nil || len(usage) == 0 {
			usageSource = audit.UsageSourceNone
		} else {
			usageSource = audit.UsageSourceEstimated
		}
	}
	event := audit.Event{Kind: audit.KindRequest, RequestID: middleware.GetRequestID(ctx), Action: "grok_request", APIKeyID: middleware.APIKeyID(ctx),
		AccountID: accountID, Model: req.Model, Channel: "grok", Provider: ProviderWeb, Status: status, Error: message, Duration: duration, Metadata: metadata,
		InputTokens: interfaceToInt(usage["prompt_tokens"]), OutputTokens: interfaceToInt(usage["completion_tokens"]), TotalTokens: interfaceToInt(usage["total_tokens"]), UsageSource: usageSource,
		CachedInputTokens: interfaceToInt(prompt["cached_tokens"]), ReasoningTokens: interfaceToInt(completion["reasoning_tokens"])}
	// Only upstream-reported usage is billed: an estimated count is this
	// gateway's own guess and must never turn into money owed.
	if cost, _ := middleware.SettleAPIKeyBilling(ctx, nil, req.Model, usageSource,
		int64(event.InputTokens), int64(event.CachedInputTokens), int64(event.OutputTokens)); cost.Model != "" {
		event.CostInUSDTicks = cost.CostInUSDTicks
		event.PricingModel = cost.Model
		event.PricingVersion = pricing.Version
	}
	logger.Log(ctx, event)
	if acc != nil && acc.ID != 0 && event.TotalTokens > 0 && h != nil && h.lb != nil && h.lb.Store != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := h.lb.Store.IncrementAccountStats(persistCtx, acc.ID, float64(event.TotalTokens), 0); err != nil {
			slog.Warn("grok web token usage persistence failed", "account_id", acc.ID, "tokens", event.TotalTokens, "error", err)
		}
	}
}

// SetConnTracker lets the Grok selectors share the deployment-wide tracker
// used by the general load balancer.
func (h *Handler) SetConnTracker(tracker loadbalancer.ConnTracker) {
	if h != nil && tracker != nil {
		h.runtimeMu.Lock()
		h.connTracker = tracker
		h.runtimeMu.Unlock()
	}
}

func (h *Handler) connTrackerSnapshot() loadbalancer.ConnTracker {
	if h == nil {
		return nil
	}
	h.runtimeMu.RLock()
	tracker := h.connTracker
	h.runtimeMu.RUnlock()
	return tracker
}

func (h *Handler) currentClient() *Client {
	return h.webClient()
}

func (h *Handler) isModelValidationCached(modelID string) bool {
	if h == nil || strings.TrimSpace(modelID) == "" {
		return false
	}
	h.modelCacheMu.RLock()
	expiresAt, ok := h.modelCache[modelID]
	h.modelCacheMu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().Before(expiresAt) {
		return true
	}
	h.modelCacheMu.Lock()
	if staleAt, ok := h.modelCache[modelID]; ok && !time.Now().Before(staleAt) {
		delete(h.modelCache, modelID)
	}
	h.modelCacheMu.Unlock()
	return false
}

func (h *Handler) cacheValidatedModel(modelID string) {
	if h == nil || strings.TrimSpace(modelID) == "" {
		return
	}
	h.modelCacheMu.Lock()
	if h.modelCache == nil {
		h.modelCache = make(map[string]time.Time)
	}
	h.modelCache[modelID] = time.Now().Add(grokModelValidationCacheTTL)
	h.modelCacheMu.Unlock()
}

// isGrokWebAccount selects only Grok Web SSO sessions. Build OAuth and
// Console SSO sessions are deliberately separate products and must never be
// sent to the legacy web transport.
func isGrokWebAccount(acc *store.Account) bool {
	return acc != nil && ProviderForAccount(acc) == ProviderWeb &&
		!strings.EqualFold(strings.TrimSpace(acc.CredentialType), "oauth")
}

// grokSSOTokenRaw returns the raw SSO credential for an account, preferring the
// client cookie and falling back to the refresh token (no normalization).
func grokSSOTokenRaw(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	raw := strings.TrimSpace(acc.ClientCookie)
	if raw == "" {
		raw = strings.TrimSpace(acc.RefreshToken)
	}
	return raw
}

func (h *Handler) selectAccount(ctx context.Context) (*store.Account, string, error) {
	if h.lb == nil {
		return nil, "", fmt.Errorf("load balancer not configured")
	}
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, nil, "grok", h.connTrackerSnapshot(), accountFilter(isGrokWebAccount))
	if err != nil {
		return nil, "", err
	}
	raw := grokSSOTokenRaw(acc)
	if NormalizeSSOToken(raw) == "" {
		return nil, "", fmt.Errorf("grok account token is empty")
	}
	return acc, raw, nil
}

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	id := normalizeModelID(modelID)
	if IsDeprecatedModelID(id) {
		return fmt.Errorf("model not found")
	}
	if h.isModelValidationCached(id) {
		return nil
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		// Without a store there is no observed catalog to validate against. A
		// compiled-in model list is not a substitute: it would accept a model no
		// active account ever advertised.
		return fmt.Errorf("model not found")
	}

	m, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", id)
	if err != nil || m == nil {
		rawID := strings.ToLower(strings.TrimSpace(modelID))
		if rawID != "" && rawID != id {
			m, err = h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", rawID)
		}
	}
	if err != nil || m == nil {
		return fmt.Errorf("model not found")
	}
	if !modelpolicy.IsVisibleGrokModel(id, m.Verified) {
		return fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return fmt.Errorf("model not available")
	}
	channel := strings.TrimSpace(m.Channel)
	if channel == "" {
		channel = "grok"
	}
	if !strings.EqualFold(channel, "grok") {
		return fmt.Errorf("model not found")
	}
	h.cacheValidatedModel(id)
	return nil
}

// ensureModelCapability applies the persisted route capability policy after
// the normal visibility/status check. Empty capability lists remain permissive
// so pre-migration records continue to work until the startup backfill runs.
func (h *Handler) ensureModelCapability(ctx context.Context, modelID, capability string) error {
	if err := h.ensureModelEnabled(ctx, modelID); err != nil {
		return err
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(modelID))
	if (err != nil || model == nil) && strings.TrimSpace(modelID) != normalizeModelID(modelID) {
		model, err = h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", strings.TrimSpace(modelID))
	}
	if err != nil || model == nil {
		return nil
	}
	if !model.SupportsCapability(capability) {
		return fmt.Errorf("model %s does not support %s", normalizeModelID(modelID), strings.ToLower(strings.TrimSpace(capability)))
	}
	return nil
}

func (h *Handler) ensureResolvedModelCapability(ctx context.Context, modelID string, spec ModelSpec, capability string) error {
	if err := h.ensureResolvedModelEnabled(ctx, modelID, spec); err != nil {
		return err
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(modelID))
	if err == nil && model != nil && !model.SupportsCapability(capability) {
		return fmt.Errorf("model %s does not support %s", normalizeModelID(modelID), strings.ToLower(strings.TrimSpace(capability)))
	}
	return nil
}

// resolveConversationModel resolves a Web route from the route table and
// overlays the operator-controlled upstream identity from the model store.
func (h *Handler) resolveConversationModel(ctx context.Context, modelID string) (ModelSpec, bool) {
	spec, _, ok := ResolveModelAlias(modelID)
	if !ok {
		return ModelSpec{}, false
	}
	return h.applyPersistedRoute(ctx, spec), true
}

// applyPersistedRoute overlays the control-plane upstream model on the static
// route shape. Media flags remain table-owned.
func (h *Handler) applyPersistedRoute(ctx context.Context, spec ModelSpec) ModelSpec {
	if h == nil || h.lb == nil || h.lb.Store == nil || strings.TrimSpace(spec.ID) == "" {
		return spec
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(spec.ID))
	if err != nil || model == nil || !strings.EqualFold(strings.TrimSpace(model.Provider), ProviderWeb) {
		return spec
	}
	if upstream := strings.TrimSpace(model.UpstreamModel); upstream != "" {
		spec.UpstreamModel = upstream
	}
	return spec
}

func (h *Handler) ensureResolvedModelEnabled(ctx context.Context, modelID string, _ ModelSpec) error {
	return h.ensureModelEnabled(ctx, modelID)
}

func modelNotFoundMessage(modelID string) string {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "The model does not exist or you do not have access to it."
	}
	return fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", modelID)
}

func modelValidationMessage(modelID string, err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	if strings.EqualFold(msg, "model not found") {
		return modelNotFoundMessage(modelID)
	}
	return msg
}

func (h *Handler) accountCapacityAvailable(acc *store.Account) bool {
	tracker := h.connTrackerSnapshot()
	limit := loadbalancer.EffectiveAccountConcurrencyLimit(acc)
	if acc == nil || limit <= 0 || tracker == nil {
		return true
	}
	return tracker.GetCount(acc.ID) < limit
}

func (h *Handler) reserveAccount(acc *store.Account) (func(), bool) {
	tracker := h.connTrackerSnapshot()
	if tracker == nil || acc == nil || acc.ID == 0 {
		if h != nil && h.base != nil {
			return h.base.TrackAccount(acc), true
		}
		return func() {}, true
	}
	if limiter, ok := tracker.(loadbalancer.LimitedConnTracker); ok {
		if !limiter.TryAcquire(acc.ID, loadbalancer.EffectiveAccountConcurrencyLimit(acc)) {
			return func() {}, false
		}
	} else {
		if !h.accountCapacityAvailable(acc) {
			return func() {}, false
		}
		tracker.Acquire(acc.ID)
	}
	return func() { tracker.Release(acc.ID) }, true
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	// Invalid parameters and missing resources are request errors, not evidence
	// that the credential is unusable. Do not poison account routing with them.
	if status := parseUpstreamStatus(err); status >= 400 && status < 500 && status != 401 && status != 402 && status != 403 && status != 429 {
		return
	}
	// Cloudflare / DPoP challenges are egress problems, not account problems.
	// Do not cool or disable the account; the egress layer must re-solve.
	if isEgressChallengeError(err) {
		return
	}
	// Team-level resource-exhausted 429: the rate limit is on the token/session,
	// not the account. Set a cooldown so the RPM window can reset. Without this,
	// unmarked sibling accounts sharing the same token immediately hit the same
	// team limit. Prefer the precise reset parsed from the 429 body (team+model
	// granularity), falling back to a 60s blanket cooldown.
	if err != nil && isResourceExhaustedError(err) && acc != nil {
		cooldown := accountpolicy.CooldownRateLimitBase
		if meta := ParseRateLimitMetadata([]byte(err.Error())); meta != nil {
			identity := ProviderForAccount(acc) + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), "")
			if remaining := teamCooldown.RetryAfterFor(meta.Scope, identity, meta.Model); remaining > 0 {
				cooldown = accountpolicy.BoundRateLimitCooldown(remaining)
			} else if meta.RetryAfter > 0 {
				cooldown = accountpolicy.BoundRateLimitCooldown(meta.RetryAfter)
			}
		}
		failureCooldown := accountpolicy.RateLimitCooldown(acc.RateLimitFailures + 1)
		if failureCooldown > cooldown {
			cooldown = failureCooldown
		}
		acc.QuotaResetAt = time.Now().Add(cooldown)
	}
	// A refusal that names one model ("access to the chat endpoint is denied",
	// "not available for model X") is a capability problem for that model only.
	// Cooling the whole credential took every other model out of the pool for
	// ten minutes; the model cooldown map exists for exactly this case
	// (grok2api marks the model, not the account).
	// A 5xx is the upstream's own trouble, not the credential's: a short hold
	// keeps the next request from immediately re-selecting the same account
	// while the upstream recovers, without marking the credential as broken.
	if acc != nil {
		if status := parseUpstreamStatus(err); status >= 500 {
			acc.QuotaResetAt = time.Now().Add(serverFaultHold)
		}
	}
	if acc != nil && isModelScopedRefusal(err) {
		if model := requestModelFromContext(ctx); model != "" {
			store.RecordModelCooldown(acc, model, time.Now().Add(modelScopedRefusalCooldown))
			if h.lb != nil && h.lb.Store != nil {
				_ = h.lb.Store.UpdateAccount(ctx, acc)
			}
			h.unbindAffinity(ctx, ProviderForAccount(acc), acc.ID)
			return
		}
	}
	// The credential cannot serve this session any more: drop the affinity so the
	// next turn picks a different account instead of coming back here.
	h.unbindAffinity(ctx, ProviderForAccount(acc), acc.ID)
	h.base.MarkAccountStatus(ctx, acc, err)
}

// modelScopedRefusalCooldown is how long one model stays out of rotation after
// the upstream refused it for this credential.
const modelScopedRefusalCooldown = 5 * time.Minute

// serverFaultHold is the short pause applied after an upstream 5xx.
const serverFaultHold = 5 * time.Second

// isModelScopedRefusal reports whether an upstream failure refused one model
// rather than the credential itself.
func isModelScopedRefusal(err error) bool {
	if err == nil {
		return false
	}
	if parseUpstreamStatus(err) != 403 {
		return false
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{
		"access to the chat endpoint is denied",
		"for model",
		"model is not available",
		"not available for model",
		"model_not_available",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isConsoleFreeQuotaExhaustedError(err error) bool {
	if err == nil || parseUpstreamStatus(err) != http.StatusTooManyRequests {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "free usage quota exceeded") ||
		strings.Contains(lower, "free usage exhausted")
}

func isResourceExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "resource-exhausted") ||
		strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "too many requests for team")
}

func (h *Handler) openChatAccountSessionForModel(ctx context.Context, spec ModelSpec) (*chatAccountSession, error) {
	return h.openChatAccountSessionForModelExcluding(ctx, nil, spec)
}

func (h *Handler) openChatAccountSessionForModelExcluding(ctx context.Context, excludeIDs []int64, spec ModelSpec) (*chatAccountSession, error) {
	return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, spec.PoolCandidates(), func(acc *store.Account) bool {
		return h.routeAllowsAccount(ctx, spec.ID, acc.ID)
	})
}

func (h *Handler) openChatAccountSessionForImagineLite(ctx context.Context, excludeIDs []int64, spec ModelSpec) (*chatAccountSession, error) {
	spec.Tier = grokTierLite
	// Basic is the minimum tier for the lite image model, so a basic credential
	// is a valid fallback rather than an excluded pool.
	return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, spec.PoolCandidates(), func(acc *store.Account) bool {
		return h.routeAllowsAccount(ctx, spec.ID, acc.ID)
	})
}

func (h *Handler) routeAllowsAccount(ctx context.Context, modelID string, accountID int64) bool {
	if h == nil || h.lb == nil || h.lb.Store == nil || accountID == 0 {
		return true
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(modelID))
	if err != nil || model == nil {
		return true
	}
	return model.AllowsAccount(accountID)
}

func (h *Handler) openChatAccountSessionExcludingWithPools(ctx context.Context, excludeIDs []int64, poolCandidates []string) (*chatAccountSession, error) {
	return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, excludeIDs, poolCandidates, nil)
}

// modelScopeKey carries the model a request is for, so account selection can
// skip an account that is cooling down for THAT model while still using it for
// the account's other models.
type modelScopeKey struct{}

// WithRequestModel records the model on the request context for account
// selection. It is threaded through the existing context so no selector
// signature has to change.
func WithRequestModel(ctx context.Context, modelID string) context.Context {
	normalized := normalizeModelID(modelID)
	if normalized == "" {
		return ctx
	}
	return context.WithValue(ctx, modelScopeKey{}, normalized)
}

func requestModelFromContext(ctx context.Context) string {
	model, _ := ctx.Value(modelScopeKey{}).(string)
	return model
}

// accountUsableForModel reports whether the account may serve the current
// request's model. The model-scoped cooldown is the persisted verdict of a
// per-model failure: a throttled model must not remove the account's other
// models from the pool.
func accountUsableForModel(ctx context.Context, acc *store.Account) bool {
	if acc == nil {
		return false
	}
	// A Build Free refusal is authoritative for the whole included-usage
	// window. Do not send the account back to the upstream every minute while
	// its confirmed 24-hour reset is still in the future.
	if ProviderForAccount(acc) == ProviderBuild && !acc.GrokFreeQuota.ResetAt.IsZero() && time.Now().Before(acc.GrokFreeQuota.ResetAt) {
		return false
	}
	// A credential parked by the quality guard stays out of rotation until its
	// cooldown ends: it answers 200 with degraded content, which no status code
	// reflects.
	if !acc.QualityCooldownUntil.IsZero() && time.Now().Before(acc.QualityCooldownUntil) {
		return false
	}
	model := requestModelFromContext(ctx)
	if model == "" {
		return true
	}
	return store.ModelCooldownRemaining(acc, model, time.Now()) == 0
}

func (h *Handler) openChatAccountSessionExcludingWithPoolsAndFilter(ctx context.Context, excludeIDs []int64, poolCandidates []string, extraFilter func(*store.Account) bool) (*chatAccountSession, error) {
	if h.lb == nil {
		return nil, fmt.Errorf("load balancer not configured")
	}
	var (
		acc     *store.Account
		err     error
		lastErr error
	)
	// App-chat/media only accepts Web SSO cookie accounts. OAuth Build and
	// Console SSO are selected by their own provider-specific selectors.
	ssoFilter := func(acc *store.Account) bool {
		if !isGrokWebAccount(acc) {
			return false
		}
		if !accountUsableForModel(ctx, acc) {
			return false
		}
		return extraFilter == nil || extraFilter(acc)
	}
	if pinnedID := h.affinityAccount(ctx, ProviderWeb); pinnedID != 0 && !containsAccountID(excludeIDs, pinnedID) && h.lb.Store != nil {
		if pinned, getErr := h.lb.Store.GetAccount(ctx, pinnedID); getErr == nil && pinned != nil && accountAffinityUsable(pinned) && h.accountCapacityAvailable(pinned) && ssoFilter(pinned) {
			raw := grokSSOTokenRaw(pinned)
			if NormalizeSSOToken(raw) != "" {
				if release, reserved := h.reserveAccount(pinned); reserved {
					return &chatAccountSession{acc: pinned, token: raw, poolCandidates: normalizeGrokPoolCandidates(poolCandidates), release: release}, nil
				}
			}
		}
	}
	candidates := normalizeGrokPoolCandidates(poolCandidates)
	if len(candidates) == 0 {
		acc, err = h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTrackerSnapshot(), accountFilter(ssoFilter))
		if err != nil {
			return nil, err
		}
	} else {
		for _, pool := range candidates {
			wantPool := pool
			acc, err = h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, excludeIDs, "grok", h.connTrackerSnapshot(), accountFilter(func(acc *store.Account) bool {
				return strings.EqualFold(grokAccountPool(acc), wantPool) && ssoFilter(acc)
			}))
			if err == nil && acc != nil {
				break
			}
			if err != nil {
				if lastErr == nil || (!carriesPoolReason(lastErr) && carriesPoolReason(err)) {
					// Keep the error that says why the pool is empty. A bare "no
					// enabled accounts available" carries no reason, so a later pool
					// that reports one (cooling down, rate limited, allowance spent,
					// busy) must replace it — otherwise the failure reaches the client
					// as a 503 that explains nothing.
					lastErr = err
				}
			}
		}
		if acc == nil {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("no enabled grok accounts available for requested pools: %s", strings.Join(candidates, ","))
		}
	}
	raw := grokSSOTokenRaw(acc)
	if NormalizeSSOToken(raw) == "" {
		return nil, fmt.Errorf("grok account token is empty")
	}
	h.bindAffinity(ctx, ProviderWeb, acc.ID)
	release, reserved := h.reserveAccount(acc)
	if !reserved {
		return h.openChatAccountSessionExcludingWithPoolsAndFilter(ctx, append(excludeIDs, acc.ID), poolCandidates, extraFilter)
	}
	return &chatAccountSession{
		acc:            acc,
		token:          raw,
		poolCandidates: candidates,
		release:        release,
	}, nil
}

func (s *chatAccountSession) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

// doSingleAccountRequest calls the Grok API once on a single account without retry switching.
func (h *Handler) doSingleAccountRequest(
	ctx context.Context,
	sess *chatAccountSession,
	payload map[string]interface{},
	shouldMarkStatus grokAccountStatusPolicy,
	callAPI func(*Client, context.Context, string, map[string]interface{}) (*http.Response, error),
) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
	}
	client := h.currentClient()
	if client == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	requestCtx := withRateLimitAccount(ctx, sess.acc)
	resp, err := callAPI(client, requestCtx, sess.token, payload)
	if err != nil {
		if shouldMarkStatus == nil || shouldMarkStatus(err) {
			h.markAccountStatus(ctx, sess.acc, err)
		}
		return nil, err
	}
	return resp, nil
}

type grokAccountStatusPolicy func(error) bool

func markAllGrokAccountStatuses(err error) bool {
	if err == nil {
		return false
	}
	// A Console Free allowance refusal is account quota exhaustion, not a shared
	// Team+Model throttle, even though xAI uses the resource-exhausted code for
	// both. It must reach markAccountStatus so the dead Console row leaves the
	// pool instead of being selected forever while its Web parent looks healthy.
	if isConsoleFreeQuotaExhaustedError(err) {
		return true
	}
	// Egress challenges and shared team rate limits are not account failures.
	if isEgressChallengeError(err) {
		return false
	}
	if isSharedGrokRateLimitError(err) {
		return false
	}
	// A generic 403 must not mark the account; only explicit account blocks do.
	if ClassifyUpstreamError(err) == UpstreamErrorGenericForbidden {
		return false
	}
	return true
}

func skipExternalAttachmentFetchGrokAccountStatus(err error) bool {
	if err == nil {
		return false
	}
	return !strings.Contains(strings.ToLower(err.Error()), "fetch url status=")
}

// doChatWithAutoSwitchRebuild calls doChat with automatic account switching and payload rebuild on failure.
func (h *Handler) doChatWithAutoSwitchRebuild(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]interface{},
	rebuild func(token string) (map[string]interface{}, error),
) (*http.Response, error) {
	return h.doAutoSwitchRequest(ctx, sess, payload, rebuild, (*Client).doChat)
}

// doAutoSwitchRequest calls the Grok API with automatic account switching on
// failure, rebuilding the request payload for the new account when rebuild is set.
func (h *Handler) doAutoSwitchRequest(
	ctx context.Context,
	sess *chatAccountSession,
	payload *map[string]interface{},
	rebuild func(token string) (map[string]interface{}, error),
	callAPI func(*Client, context.Context, string, map[string]interface{}) (*http.Response, error),
) (*http.Response, error) {
	if sess == nil || strings.TrimSpace(sess.token) == "" {
		return nil, fmt.Errorf("empty chat session")
	}
	if payload == nil {
		return nil, fmt.Errorf("empty payload")
	}
	client := h.currentClient()
	if client == nil {
		return nil, fmt.Errorf("grok client not configured")
	}
	return h.retryWithAccountSwitch(ctx, sess, 100*time.Millisecond,
		func() (*http.Response, error) {
			return callAPI(client, withRateLimitAccount(ctx, sess.acc), sess.token, *payload)
		},
		func(used []int64) (*chatAccountSession, error) {
			return h.openChatAccountSessionExcludingWithPools(ctx, used, sess.poolCandidates)
		},
		func() error {
			if rebuild == nil {
				return nil
			}
			newPayload, rbErr := rebuild(sess.token)
			if rbErr != nil {
				return rbErr
			}
			*payload = newPayload
			return nil
		})
}

// isEgressChallengeError reports whether an error is a Cloudflare interstitial
// or DPoP proof challenge. These are egress/clearance problems, not account
// problems: switching accounts (or cooling the account) is wrong.
func isEgressChallengeError(err error) bool {
	if err == nil {
		return false
	}
	kind := ClassifyUpstreamError(err)
	return kind == UpstreamErrorCloudflareChallenge || kind == UpstreamErrorDPoPChallenge
}

func shouldSwitchGrokAccount(err error) bool {
	if err == nil {
		return false
	}
	// Cloudflare / DPoP challenges should not drain the account pool: switching
	// to another account hits the same wall. Leave to the egress layer instead.
	if isEgressChallengeError(err) {
		return false
	}
	// Response-aware classification: only an explicit account block switches
	// accounts. A generic 403 (feature/plan/permission) is not an account
	// failure.
	switch ClassifyUpstreamError(err) {
	case UpstreamErrorAccountBlock:
		return true
	case UpstreamErrorGenericForbidden:
		return false
	}
	status := apperrors.ClassifyAccountStatus(err.Error())
	if status == "401" || status == "429" {
		return true
	}
	if isSharedGrokRateLimitError(err) {
		return true
	}
	if upstreamStatus := parseUpstreamStatus(err); upstreamStatus == http.StatusBadGateway ||
		upstreamStatus == http.StatusServiceUnavailable ||
		upstreamStatus == http.StatusGatewayTimeout ||
		upstreamStatus == http.StatusInternalServerError {
		return true
	}

	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "timeout"),
		strings.Contains(lower, "deadline exceeded"),
		strings.Contains(lower, "connection reset"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "broken pipe"),
		strings.HasSuffix(lower, ": eof"),
		lower == "eof":
		return true
	default:
		return false
	}
}

func isSharedGrokRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	if parseUpstreamStatus(err) != http.StatusTooManyRequests && !strings.Contains(lower, "too many requests") {
		return false
	}
	// Only a parsed Team+Model response is shared. A bare 429/"too many
	// requests" is account-scoped in grok2api and must cool the selected account;
	// treating it as shared left every account looking healthy while requests
	// repeatedly rotated through a pool of throttled credentials.
	if ParseRateLimitMetadata([]byte(err.Error())) != nil {
		return true
	}
	// Errors emitted by waitScopedRateLimit are synthetic: the structured
	// response was parsed on the preceding attempt and the team/model cooldown
	// is already registered, so there is no response body left to parse here.
	return strings.Contains(lower, "body=too_many_requests team ") &&
		strings.Contains(lower, " cooling down; retry-after=")
}

type grokQuotaSyncJob struct {
	account  store.Account
	headers  http.Header
	requests int64
}

func (h *Handler) syncGrokQuota(acc *store.Account, headers http.Header) {
	if acc == nil || h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	h.quotaOnce.Do(func() {
		h.quotaPending = make(map[int64]grokQuotaSyncJob)
		h.quotaWake = make(chan struct{}, 1)
		go h.runGrokQuotaSync()
	})
	h.quotaMu.Lock()
	job := h.quotaPending[acc.ID]
	job.account = *acc
	job.headers = headers.Clone()
	job.requests++
	h.quotaPending[acc.ID] = job
	h.quotaMu.Unlock()
	select {
	case h.quotaWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runGrokQuotaSync() {
	for range h.quotaWake {
		for {
			h.quotaMu.Lock()
			var accountID int64
			var job grokQuotaSyncJob
			for id, pending := range h.quotaPending {
				accountID, job = id, pending
				delete(h.quotaPending, id)
				break
			}
			h.quotaMu.Unlock()
			if accountID == 0 {
				break
			}
			h.persistGrokQuotaSync(job)
		}
	}
}

func (h *Handler) persistGrokQuotaSync(job grokQuotaSyncJob) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.lb.Store.IncrementAccountStats(ctx, job.account.ID, 0, job.requests); err != nil {
		slog.Warn("grok usage touch failed", "account_id", job.account.ID, "error", err)
	}

	headers := job.headers
	info := parseRateLimitInfo(headers)
	requests := parseBuildRateLimitWindow(headers, "requests")
	tokens := parseBuildRateLimitWindow(headers, "tokens")
	hasBuildHeaders := requests.HasLimit || requests.HasRemaining || !requests.ResetAt.IsZero() || tokens.HasLimit || tokens.HasRemaining || !tokens.ResetAt.IsZero()
	if info == nil && !hasBuildHeaders {
		provider := ProviderForAccount(&job.account)
		if _, err := h.lb.Store.ConsumeGrokQuota(ctx, job.account.ID, provider, float64(job.requests)); err != nil {
			slog.Warn("grok local quota decrement failed", "account_id", job.account.ID, "error", err)
		}
		return
	}
	latest, err := h.lb.Store.GetAccount(ctx, job.account.ID)
	if err != nil || latest == nil {
		slog.Warn("grok quota account reload failed", "account_id", job.account.ID, "error", err)
		return
	}
	NormalizeProvider(latest)
	provider := ProviderForAccount(latest)
	changed := false
	if provider == ProviderBuild {
		changed = ApplyBuildRateLimits(latest, headers)
	} else {
		changed = info != nil && ApplyQuotaInfo(latest, info)
	}
	if changed {
		if err := h.lb.Store.UpdateAccount(ctx, latest); err != nil {
			slog.Warn("grok quota update failed", "account_id", latest.ID, "error", err)
		}
	}
}
