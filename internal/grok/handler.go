package grok

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/modelpolicy"
	"orchids-api/internal/pricing"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"strings"
	"sync"
	"time"

	apperrors "orchids-api/internal/errors"
)

const grokModelValidationCacheTTL = 3 * time.Second

type Handler struct {
	base          *handler.BaseHandler
	runtimeMu     sync.RWMutex
	cfg           *config.Config
	lb            *loadbalancer.LoadBalancer
	cliClient     *CLIClient
	connTracker   loadbalancer.ConnTracker
	modelCacheMu  sync.RWMutex
	modelCache    map[string]time.Time
	sessionMu     sync.Mutex
	affinityLocks [64]sync.Mutex
	affinity      map[string]sessionAffinityEntry
	replay        map[string]reasoningReplayEntry
	replayGen     map[string]uint64
	instanceID    string
	auditLogger   audit.Logger
	// compactionCode seals and opens gateway-owned remote-v2 compaction state.
	// Nil (no credential key configured) means the gateway cannot own a summary
	// and compaction requests stay a plain upstream forward.
	compactionMu   sync.RWMutex
	compactionCode *responses.CompactionCodec

	quotaOnce    sync.Once
	quotaMu      sync.Mutex
	quotaPending map[int64]grokQuotaSyncJob
	quotaWake    chan struct{}
}

type chatAccountSession struct {
	acc            *store.Account
	token          string
	poolCandidates []string
	release        func()
}

func NewHandler(cfg *config.Config, lb *loadbalancer.LoadBalancer) *Handler {
	cliClient := NewCLIClient(cfg)
	if lb != nil {
		cliClient.SetAccountStore(lb.Store)
		if lb.Store != nil {
			configureDistributedGrokLimits(lb.Store.RedisClient(), lb.Store.RedisPrefix())
		}
	}
	instanceID := "grok-" + randomHex(16)
	if cfg != nil && strings.TrimSpace(cfg.DeploymentInstance) != "" {
		instanceID = strings.TrimSpace(cfg.DeploymentInstance)
	}
	h := &Handler{
		base:        handler.NewBaseHandler(lb),
		cfg:         cfg,
		lb:          lb,
		cliClient:   cliClient,
		connTracker: loadbalancer.NewMemoryConnTracker(),
		modelCache:  make(map[string]time.Time),
		affinity:    make(map[string]sessionAffinityEntry),
		replay:      make(map[string]reasoningReplayEntry),
		replayGen:   make(map[string]uint64),
		instanceID:  instanceID,
		auditLogger: audit.NewNopLogger(),
	}
	return h
}

// SetConfig replaces the immutable config and the clients whose transports are
// derived from it. Existing requests retain their client pointers; new requests
// immediately observe the new proxy, timeout and endpoint settings.
func (h *Handler) SetConfig(cfg *config.Config) {
	if h == nil || cfg == nil {
		return
	}
	cliClient := NewCLIClient(cfg)
	if h.lb != nil {
		cliClient.SetAccountStore(h.lb.Store)
	}
	h.runtimeMu.Lock()
	h.cfg = cfg
	h.cliClient = cliClient
	h.runtimeMu.Unlock()
}

func (h *Handler) configSnapshot() *config.Config {
	if h == nil {
		return nil
	}
	return util.ReadSnapshot(&h.runtimeMu, &h.cfg)
}

func (h *Handler) buildClient() *CLIClient {
	if h == nil {
		return nil
	}
	return util.ReadSnapshot(&h.runtimeMu, &h.cliClient)
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

func (h *Handler) auditAttempt(ctx context.Context, acc *store.Account, provider string, attempt int, started time.Time, err error, stages ...string) {
	stage := "request"
	if len(stages) > 0 {
		stage = stages[0]
	}
	h.auditAttemptDiagnostic(ctx, acc, provider, attempt, started, err, stage, nil, nil, "")
}

func (h *Handler) auditChatOutcome(ctx context.Context, acc *store.Account, req *chatwire.Request, result chatOutcome) {
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
	metadata := map[string]interface{}{"finish_reason": result.Finish, "visible_output": result.SawText}
	addReasoningDiagnostics(ctx, metadata)
	duration := int64(0)
	if !req.StartedAt.IsZero() {
		duration = time.Since(req.StartedAt).Milliseconds()
		if !result.FirstToken.IsZero() {
			metadata["first_token_ms"] = result.FirstToken.Sub(req.StartedAt).Milliseconds()
		}
	}
	accountID := int64(0)
	provider := ""
	if acc != nil {
		accountID = acc.ID
		provider = ProviderForAccount(acc)
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
		AccountID: accountID, Model: req.Model, Channel: "grok", Provider: provider, Status: status, Error: message, Duration: duration, Metadata: metadata,
		InputTokens: interfaceToInt(usage["prompt_tokens"]), OutputTokens: interfaceToInt(usage["completion_tokens"]), TotalTokens: interfaceToInt(usage["total_tokens"]), UsageSource: usageSource,
		CachedInputTokens: interfaceToInt(prompt["cached_tokens"]), ReasoningTokens: interfaceToInt(completion["reasoning_tokens"])}
	// Price the turn and book it against the client key's reservation. Only
	// upstream-reported usage is billed: an estimated count is this gateway's
	// own guess and must never turn into money owed.
	if cost, priced := middleware.SettleAPIKeyBilling(ctx, nil, req.Model, usageSource,
		int64(event.InputTokens), int64(event.CachedInputTokens), int64(event.OutputTokens)); priced {
		event.CostInUSDTicks = cost.CostInUSDTicks
		event.PricingModel = cost.Model
		event.PricingVersion = pricing.Version
	}
	logger.Log(ctx, event)
	// Grok owns its native request handlers, so it bypasses the generic handler's
	// account-usage accumulator. Persist the same usage attached to the audit
	// event here; otherwise request_count advances through syncGrokQuota while
	// tokens_today and usage_total remain permanently zero.
	if acc != nil && acc.ID != 0 && event.TotalTokens > 0 && h != nil && h.lb != nil && h.lb.Store != nil {
		accountID := acc.ID
		if err := h.lb.Store.IncrementAccountStats(ctx, accountID, float64(event.TotalTokens), 0); err != nil {
			slog.Warn("grok token usage persistence failed", "account_id", accountID, "runtime_account_id", acc.ID, "tokens", event.TotalTokens, "error", err)
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
	return util.ReadSnapshot(&h.runtimeMu, &h.connTracker)
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

func (h *Handler) ensureModelEnabled(ctx context.Context, modelID string) error {
	id := normalizeModelID(modelID)
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
	if err != nil || m == nil || !modelpolicy.IsVisibleGrokModel(id, m.Verified) {
		return fmt.Errorf("model not found")
	}
	if !m.Status.Enabled() {
		return fmt.Errorf("model not available")
	}
	channel := strings.TrimSpace(m.Channel)
	channel = util.FirstNonEmptyUntrimmed(channel, "grok")
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

// resolveConversationModel resolves the built-in Build catalog and
// account-discovered Build models. A discovered model is routable only when at
// least one enabled Build account advertises it, so arbitrary client strings
// can never turn into upstream model probes.
func (h *Handler) resolveConversationModel(ctx context.Context, modelID string) (ModelSpec, bool) {
	id := normalizeModelID(modelID)
	if id == "" {
		return ModelSpec{}, false
	}
	if spec, effort, ok := ResolveModelAlias(modelID); ok {
		spec.AliasReasoningEffort = effort
		return h.applyPersistedRoute(ctx, spec), true
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return ModelSpec{}, false
	}
	accounts, err := h.lb.Store.GetEnabledAccounts(ctx)
	if err != nil {
		return ModelSpec{}, false
	}
	for _, acc := range accounts {
		if ProviderForAccount(acc) != ProviderBuild || len(acc.GrokModels) == 0 {
			continue
		}
		for _, candidate := range acc.GrokModels {
			if strings.EqualFold(strings.TrimSpace(candidate), id) {
				spec := ModelSpec{ID: id, Name: id, UpstreamModel: strings.TrimSpace(candidate), Upstream: UpstreamCLI}
				return h.applyPersistedRoute(ctx, spec), true
			}
		}
	}
	return ModelSpec{}, false
}

// applyPersistedRoute overlays the control-plane route on the static model
// shape. Media/voice flags remain catalog-owned, while provider and upstream
// identity are operator-controlled and survive restarts in the model store.
func (h *Handler) applyPersistedRoute(ctx context.Context, spec ModelSpec) ModelSpec {
	if h == nil || h.lb == nil || h.lb.Store == nil || strings.TrimSpace(spec.ID) == "" {
		return spec
	}
	model, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", normalizeModelID(spec.ID))
	if err != nil || model == nil {
		return spec
	}
	upstream := strings.TrimSpace(model.UpstreamModel)
	upstream = util.FirstNonEmptyUntrimmed(upstream, spec.UpstreamModel)
	// Build is the only Grok plane this gateway serves, so a stored route row can
	// only refine the upstream model name.
	if strings.EqualFold(strings.TrimSpace(model.Provider), ProviderBuild) && upstream != "" {
		spec.Upstream = UpstreamCLI
		spec.UpstreamModel = upstream
	}
	return spec
}

func (h *Handler) ensureResolvedModelEnabled(ctx context.Context, modelID string, spec ModelSpec) error {
	err := h.ensureModelEnabled(ctx, modelID)
	if err == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
		return err
	}
	// Dynamic Build capabilities are authoritative even before the admin model
	// table has been reconciled by the next catalog refresh.
	if spec.Upstream == UpstreamCLI {
		if dynamic, ok := h.resolveConversationModel(ctx, modelID); ok && strings.EqualFold(dynamic.UpstreamModel, spec.UpstreamModel) {
			return nil
		}
	}
	return err
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
	if status := upstreamStatus(err); status >= 400 && status < 500 && status != 401 && status != 402 && status != 403 && status != 429 {
		return
	}
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) && oauthErr.status == http.StatusUnauthorized && acc != nil {
		acc.OAuthAccessToken = ""
		acc.OAuthRefreshToken = ""
		acc.OAuthExpiresAt = time.Time{}
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
	// (the cooldown marks the model, not the account).
	// A 5xx is the upstream's own trouble, not the credential's: a short hold
	// keeps the next request from immediately re-selecting the same account
	// while the upstream recovers, without marking the credential as broken.
	if acc != nil {
		if status := upstreamStatus(err); status >= 500 {
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
	if err == nil || upstreamStatus(err) != 403 {
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

func isResourceExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "resource-exhausted") ||
		strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "too many requests for team")
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

func (s *chatAccountSession) Close() {
	if s == nil || s.release == nil {
		return
	}
	s.release()
	s.release = nil
}

func markAllGrokAccountStatuses(err error) bool {
	if err == nil {
		return false
	}
	// Shared team rate limits and preflight cooldowns are not account failures.
	if isSharedGrokRateLimitError(err) {
		return false
	}
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) && oauthErr.status == http.StatusUnauthorized {
		return true
	}
	// A generic 403 must not mark the account; only explicit account blocks do.
	return !(ClassifyUpstreamError(err) == UpstreamErrorGenericForbidden)
}

func shouldSwitchGrokAccount(err error) bool {
	if err == nil {
		return false
	}
	var oauthErr *cliOAuthError
	if errors.As(err, &oauthErr) && oauthErr.status == http.StatusUnauthorized {
		return true
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
	if status == "401" || status == "429" || isSharedGrokRateLimitError(err) {
		return true
	}
	if status := upstreamStatus(err); status == http.StatusBadGateway ||
		status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout ||
		status == http.StatusInternalServerError {
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
	var synthetic *syntheticCooldownError
	if errors.As(err, &synthetic) {
		return true
	}
	lower := strings.ToLower(err.Error())
	if upstreamStatus(err) != http.StatusTooManyRequests && !strings.Contains(lower, "too many requests") {
		return false
	}
	// Only a parsed Team+Model response is shared. A bare 429/"too many
	// requests" is account-scoped and must cool the selected account;
	// treating it as shared left every account looking healthy while requests
	// repeatedly rotated through a pool of throttled credentials.
	if ParseRateLimitMetadata([]byte(err.Error())) != nil {
		return true
	}
	return false
}

func upstreamHTTPResponseStatus(err error) int {
	if status := upstreamStatus(err); status >= 400 && status <= 599 {
		return status
	}
	return http.StatusBadGateway
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
