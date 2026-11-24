package web

import (
	"context"
	"fmt"
	"log/slog"
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/ratelimit"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxEditImageBytes = 50 * 1024 * 1024

var cacheBaseDir = filepath.Join("data", "tmp")

// Handler serves the Grok Web (grok.com SSO) plane: AppChat conversations,
// Imagine images, videos, media inputs, the media cache and the SSO account
// tools. The parent grok channel dispatches Web models here.
type Handler struct {
	base        *grokbase.AccountTracker
	runtimeMu   sync.RWMutex
	cfg         *config.Config
	lb          *loadbalancer.LoadBalancer
	client      *Client
	connTracker loadbalancer.ConnTracker
	affinity    grokbase.Affinity
	instanceID  string
	auditLogger audit.Logger

	quota grokbase.QuotaQueue

	// videoBackend serves video routes of another plane; see video_backend.go.
	videoBackend VideoBackend
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
	instanceID := "grok-web-" + util.RandomHex(16)
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
		if acc.IsGrokWeb() && acc.Enabled && store.AccountAuthActive(acc) && !accountObservesRoutes(acc, time.Now()) {
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
	grokbase.LogChatOutcome(ctx, logger, h.affinityStore(), acc, &req.Request, grokbase.ChatAudit{
		Provider: ProviderWeb, Finish: result.Finish, Err: result.Err, FirstToken: result.FirstToken,
		Usage: result.Usage, UsageSource: result.UsageSource,
	})
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
	acc, err := h.lb.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, nil, "grok", h.connTrackerSnapshot(), accountFilter((*store.Account).IsGrokWeb))
	if err != nil {
		return nil, "", err
	}
	raw := grokSSOTokenRaw(acc)
	if NormalizeSSOToken(raw) == "" {
		return nil, "", fmt.Errorf("grok account token is empty")
	}
	return acc, raw, nil
}

// ensureModelCapability checks visibility, status and capability on the same
// persisted route, so a model taken offline stops serving immediately.
func (h *Handler) ensureModelCapability(ctx context.Context, modelID, capability string) error {
	if h == nil {
		return fmt.Errorf("model not found")
	}
	id := normalizeModelID(modelID)
	m, err := grokbase.EnabledModel(ctx, h.affinityStore(), id)
	if err != nil {
		return err
	}
	if !m.SupportsCapability(capability) {
		return fmt.Errorf("model %s does not support %s", id, capability)
	}
	return nil
}

// resolveConversationModel resolves a Web route from the route table and
// overlays the operator-controlled upstream identity from the model store.
func (h *Handler) resolveConversationModel(ctx context.Context, modelID string) (ModelSpec, bool) {
	spec, ok := ResolveModel(modelID)
	if !ok {
		return ModelSpec{}, false
	}
	return h.applyPersistedRoute(ctx, spec), true
}

// applyPersistedRoute overlays the control-plane upstream model on the static
// route shape. Media flags remain table-owned.
func (h *Handler) applyPersistedRoute(ctx context.Context, spec ModelSpec) ModelSpec {
	if h != nil {
		spec.UpstreamModel = grokbase.UpstreamRoute(ctx, h.affinityStore(), normalizeModelID(spec.ID), ProviderWeb, spec.UpstreamModel)
	}
	return spec
}

func (h *Handler) accountCapacityAvailable(acc *store.Account) bool {
	return grokbase.AccountCapacityAvailable(h.connTrackerSnapshot(), acc)
}

func (h *Handler) reserveAccount(ctx context.Context, acc *store.Account) (func(), bool) {
	var base *grokbase.AccountTracker
	if h != nil {
		base = h.base
	}
	return base.Reserve(ctx, h.connTrackerSnapshot(), acc)
}

func (h *Handler) markAccountStatus(ctx context.Context, acc *store.Account, err error) {
	// Invalid parameters and missing resources are request errors, not evidence
	// that the credential is unusable. Do not poison account routing with them.
	status := parseUpstreamStatus(err)
	if grokbase.RequestErrorOnly(status) {
		return
	}
	// Cloudflare / DPoP challenges are egress problems, not account problems.
	// Do not cool or disable the account; the egress layer must re-solve.
	if isEgressChallengeError(err) {
		return
	}
	if acc == nil {
		return
	}
	var message string
	if err != nil {
		message = err.Error()
	}
	provider := acc.GrokPlane()
	modelOnly := grokbase.ApplyAccountHold(ctx, h.affinityStore(), acc, status, message, requestModelFromContext(ctx), func() string {
		return provider + ":" + rateLimitIdentity(withRateLimitAccount(ctx, acc), "")
	})
	h.unbindAffinity(ctx, provider, acc.ID)
	if !modelOnly {
		h.base.MarkAccountStatus(ctx, acc, err)
	}
}
