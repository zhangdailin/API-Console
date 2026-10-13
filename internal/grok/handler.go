package grok

import (
	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
	"strings"
	"sync"
	"time"
)

const grokModelValidationCacheTTL = 3 * time.Second

type Handler struct {
	base          *baseHandler
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
	instanceID := "grok-" + util.RandomHex(16)
	if cfg != nil && strings.TrimSpace(cfg.DeploymentInstance) != "" {
		instanceID = strings.TrimSpace(cfg.DeploymentInstance)
	}
	h := &Handler{
		base:        newBaseHandler(lb),
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
