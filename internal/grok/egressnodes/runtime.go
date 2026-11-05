package egressnodes

import (
	"context"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// RegistryScope maps a request scope used by the upstream clients to a node
// scope: "cli" (Build), "app_chat" (Web), "asset" (Web asset downloads).
func RegistryScope(scope string) string {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "cli", "build", store.GrokEgressScopeBuild:
		return store.GrokEgressScopeBuild
	case "asset", store.GrokEgressScopeWebAsset:
		return store.GrokEgressScopeWebAsset
	default:
		return store.GrokEgressScopeWeb
	}
}

// SnapshotFor returns the snapshot a manager should route with: the process
// registry once initialized, otherwise the nodes still defined in the
// configuration file (tests and processes without a store).
func SnapshotFor(cfg *config.Config) *Snapshot {
	if Default() != nil {
		return Current()
	}
	return SnapshotFromConfig(cfg)
}

var legacyCache sync.Map // node definitions -> *Snapshot

// SnapshotFromConfig builds a snapshot from grok_egress_nodes. Legacy nodes
// keep their names as IDs; scope "all" serves both Build and Web.
func SnapshotFromConfig(cfg *config.Config) *Snapshot {
	if cfg == nil || !cfg.GrokEgressEnabled {
		return emptySnapshot
	}
	var key strings.Builder
	for _, legacy := range cfg.GrokEgressNodes {
		key.WriteString(legacy.Name + "\x00" + legacy.URL + "\x00" + legacy.Scope + "\x01")
	}
	if cached, ok := legacyCache.Load(key.String()); ok {
		return cached.(*Snapshot)
	}
	var nodes []*store.GrokEgressNode
	for _, legacy := range cfg.GrokEgressNodes {
		var scopes []string
		switch strings.ToLower(strings.TrimSpace(legacy.Scope)) {
		case "cli", "build":
			scopes = []string{store.GrokEgressScopeBuild}
		case "app_chat", "web":
			scopes = []string{store.GrokEgressScopeWeb}
		default:
			scopes = []string{store.GrokEgressScopeBuild, store.GrokEgressScopeWeb}
		}
		for _, scope := range scopes {
			name := strings.TrimSpace(legacy.Name)
			nodes = append(nodes, &store.GrokEgressNode{ID: name, Name: name, Scope: scope, Enabled: true, ProxyURL: strings.TrimSpace(legacy.URL)})
		}
	}
	ops := store.DefaultGrokEgressOperations()
	for _, scope := range store.GrokEgressScopes {
		// A configured pool was fail-closed: no healthy node meant an error.
		ops.Fallbacks[scope] = store.GrokEgressFallback{Mode: "none"}
	}
	snap := buildSnapshot(nodes, nil, ops, nil)
	// Duplicate IDs across scopes collapse in byID; routing uses Nodes.
	legacyCache.Store(key.String(), snap)
	return snap
}

// Health is a node's live routing health, reported by the managers.
type Health struct {
	Score         float64
	Failures      int
	CooldownUntil time.Time
	LastError     string
	UpdatedAt     time.Time
}

var healthByNode sync.Map // node ID -> Health

// ReportHealth records a node's health for the admin view.
func ReportHealth(nodeID string, health Health) {
	if nodeID == "" {
		return
	}
	health.UpdatedAt = time.Now()
	healthByNode.Store(nodeID, health)
}

// HealthOf returns a node's last reported health.
func HealthOf(nodeID string) (Health, bool) {
	value, ok := healthByNode.Load(nodeID)
	if !ok {
		return Health{}, false
	}
	return value.(Health), true
}

// ClearanceInfo describes the Cloudflare clearance a Web node holds.
type ClearanceInfo struct {
	Mode        string    `json:"mode"`
	Status      string    `json:"status"` // manual|ready|stale|missing|invalid|not_applicable
	RefreshedAt time.Time `json:"refreshed_at,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// ClearanceHooks lets the Web egress manager, which owns the clearance cache,
// serve the admin view without this package importing it.
type ClearanceHooks struct {
	Inspect func(nodeID string) ClearanceInfo
	Refresh func(ctx context.Context, nodeID string) error
}

var clearanceHooks struct {
	sync.RWMutex
	hooks ClearanceHooks
}

// SetClearanceHooks installs the clearance hooks.
func SetClearanceHooks(hooks ClearanceHooks) {
	clearanceHooks.Lock()
	clearanceHooks.hooks = hooks
	clearanceHooks.Unlock()
}

func currentClearanceHooks() ClearanceHooks {
	clearanceHooks.RLock()
	defer clearanceHooks.RUnlock()
	return clearanceHooks.hooks
}

// UsesClearance reports whether nodes of scope carry Cloudflare clearance.
func UsesClearance(scope string) bool {
	return scope == store.GrokEgressScopeWeb || scope == store.GrokEgressScopeWebAsset
}
