package web

import (
	"context"
	"fmt"
	"log/slog"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
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
)

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

const grokModelValidationCacheTTL = 3 * time.Second

// Handler serves the Grok Web (grok.com SSO) plane: AppChat conversations,
// Imagine images, videos, media inputs, the media cache and the SSO account
// tools. The parent grok channel dispatches Web models here.
type Handler struct {
	base         *grokbase.AccountTracker
	runtimeMu    sync.RWMutex
	cfg          *config.Config
	lb           *loadbalancer.LoadBalancer
	client       *Client
	connTracker  loadbalancer.ConnTracker
	modelCacheMu sync.RWMutex
	modelCache   map[string]time.Time
	affinity     grokbase.Affinity
	instanceID   string
	auditLogger  audit.Logger

	quota grokbase.QuotaQueue
}

// chatOutcome is what one Web turn produced, for the audit journal.
type chatOutcome struct {
	Usage       map[string]any
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
		ratelimit.ConfigureRedis(lb.Store.RedisClient(), lb.Store.RedisPrefix())
	}
	instanceID := "grok-web-" + randomHex(16)
	if cfg != nil && strings.TrimSpace(cfg.DeploymentInstance) != "" {
		instanceID = strings.TrimSpace(cfg.DeploymentInstance)
	}
	if cfg != nil && strings.TrimSpace(cfg.MediaDir) != "" {
		cacheBaseDir = strings.TrimSpace(cfg.MediaDir)
	}
	h := &Handler{
		base:        grokbase.NewAccountTracker(lb),
		cfg:         cfg,
		lb:          lb,
		client:      New(cfg),
		connTracker: loadbalancer.NewMemoryConnTracker(),
		modelCache:  make(map[string]time.Time),
		instanceID:  instanceID,
		auditLogger: audit.NewNopLogger(),
	}
	if lb != nil && lb.Store != nil {
		// Re-derive the published Web rows from the stored observations, so a
		// route-metadata change reaches rows published by an earlier build.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := PublishRoutes(ctx, lb.Store); err != nil {
				slog.Warn("grok web route publication at startup failed", "error", err)
			}
		}()
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
	prompt, _ := usage["prompt_tokens_details"].(map[string]any)
	completion, _ := usage["completion_tokens_details"].(map[string]any)
	metadata := map[string]any{"finish_reason": result.Finish}
	grokbase.AddReasoningDiagnostics(ctx, metadata)
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

// isGrokWebAccount selects only Grok Web SSO sessions. Build OAuth sessions are
// a separate product and must never be sent to the legacy web transport.
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
	return grokbase.AccountCapacityAvailable(h.connTrackerSnapshot(), acc)
}

func (h *Handler) reserveAccount(acc *store.Account) (func(), bool) {
	var base *grokbase.AccountTracker
	if h != nil {
		base = h.base
	}
	return base.Reserve(h.connTrackerSnapshot(), acc)
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
		if meta := ratelimit.ParseMetadata([]byte(err.Error())); meta != nil {
			identity := ProviderForAccount(acc) + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), "")
			if remaining := ratelimit.Cooldowns.RetryAfterFor(meta.Scope, identity, meta.Model); remaining > 0 {
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

func isResourceExhaustedError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "resource-exhausted") ||
		strings.Contains(lower, "resource_exhausted") ||
		strings.Contains(lower, "too many requests for team")
}
