// Package egressnodes is the Grok channel's egress node registry: the exit
// nodes, proxy profiles and automation settings operators manage from the
// configuration page, resolved into an immutable routing snapshot that the
// Build and Web egress managers read on every request.
//
// The registry is long-lived. Configuration saves rebuild the upstream clients
// but never the registry, so node health, bindings and Cloudflare clearance
// survive them. Writes bump a version counter in Redis; every instance polls it
// and reloads, so a node edited on one instance routes the same everywhere.
package egressnodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// Node is a resolved, routable exit.
type Node struct {
	ID        string
	Name      string
	Scope     string
	ProxyURL  string // effective proxy address (profile resolved); may hold {account}
	Pool      bool
	Capacity  int
	UserAgent string
	Cookies   string
	SourceID  string
}

// Direct reports whether the node has no proxy.
func (n Node) Direct() bool { return strings.TrimSpace(n.ProxyURL) == "" }

// ProxyFor returns the proxy URL for an account affinity, substituting the
// {account} placeholder rotating-pool providers use to pin a session.
func (n Node) ProxyFor(affinity string) (*url.URL, error) {
	raw := strings.TrimSpace(n.ProxyURL)
	if raw == "" {
		return nil, nil
	}
	if strings.Contains(raw, "{account}") {
		raw = strings.ReplaceAll(raw, "{account}", shortHash(affinity)[:12])
	}
	return ParseProxyURL(raw)
}

// Snapshot is one immutable view of the registry.
type Snapshot struct {
	Nodes      []Node
	byID       map[string]Node
	Invalid    map[string]string // node ID -> why it cannot route
	Operations store.GrokEgressOperations
	Bindings   map[string]string // scope|account -> node ID
	LoadedAt   time.Time
}

var emptySnapshot = &Snapshot{byID: map[string]Node{}, Invalid: map[string]string{}, Operations: store.DefaultGrokEgressOperations(), Bindings: map[string]string{}}

// Node returns a routable node by ID.
func (s *Snapshot) Node(id string) (Node, bool) {
	if s == nil {
		return Node{}, false
	}
	node, ok := s.byID[id]
	return node, ok
}

// Candidates returns the enabled nodes serving scope. Web asset downloads use
// asset nodes when there are any and Web nodes otherwise.
func (s *Snapshot) Candidates(scope string) []Node {
	if s == nil {
		return nil
	}
	var out []Node
	for _, node := range s.Nodes {
		if node.Scope == scope {
			out = append(out, node)
		}
	}
	if len(out) == 0 && scope == store.GrokEgressScopeWebAsset {
		return s.Candidates(store.GrokEgressScopeWeb)
	}
	return out
}

// Fallback returns what scope does when no node can serve.
func (s *Snapshot) Fallback(scope string) store.GrokEgressFallback {
	if s == nil {
		return store.GrokEgressFallback{Mode: "direct"}
	}
	if fallback, ok := s.Operations.Fallbacks[scope]; ok {
		return fallback
	}
	return store.GrokEgressFallback{Mode: "direct"}
}

// Routes reports whether requests for scope go through the egress managers:
// the scope has nodes, or its fallback is not the general proxy.
func (s *Snapshot) Routes(scope string) bool {
	return len(s.Candidates(scope)) > 0 || s.Fallback(scope).Mode != "direct"
}

// Binding returns the node an account is pinned to for scope.
func (s *Snapshot) Binding(scope, account string) string {
	if s == nil || account == "" {
		return ""
	}
	return s.Bindings[scope+"|"+account]
}

// BoundCount counts the accounts pinned to each node.
func (s *Snapshot) BoundCount() map[string]int {
	out := map[string]int{}
	if s == nil {
		return out
	}
	for _, node := range s.Bindings {
		out[node]++
	}
	return out
}

// Registry owns the snapshot and its reload loop.
type Registry struct {
	store    *store.Store
	snapshot atomic.Pointer[Snapshot]
	version  atomic.Int64
	reloadMu sync.Mutex
	config   atomic.Pointer[config.Config]
}

var current atomic.Pointer[Registry]

// Default returns the process registry, or nil before Init.
func Default() *Registry { return current.Load() }

// Current returns the process snapshot; never nil.
func Current() *Snapshot {
	if r := Default(); r != nil {
		if snap := r.snapshot.Load(); snap != nil {
			return snap
		}
	}
	return emptySnapshot
}

// Init creates the process registry, migrates nodes still defined in the
// configuration file, loads the first snapshot and starts the reload loop.
func Init(ctx context.Context, st *store.Store, cfg *config.Config) *Registry {
	r := &Registry{store: st}
	r.snapshot.Store(emptySnapshot)
	r.config.Store(cfg)
	current.Store(r)
	if st == nil {
		return r
	}
	if err := migrateConfigNodes(ctx, st, cfg); err != nil {
		slog.Warn("egress: migrating configured nodes failed", "error", err)
	}
	if err := r.Reload(ctx); err != nil {
		slog.Warn("egress: initial load failed", "error", err)
	}
	go r.watch(ctx)
	go r.runJobs(ctx)
	return r
}

// SetConfig records the runtime configuration for probes and clearance.
func (r *Registry) SetConfig(cfg *config.Config) {
	if r != nil && cfg != nil {
		r.config.Store(cfg)
	}
}

// Config returns the last runtime configuration.
func (r *Registry) Config() *config.Config {
	if r == nil {
		return nil
	}
	return r.config.Load()
}

// Store returns the backing store.
func (r *Registry) Store() *store.Store {
	if r == nil {
		return nil
	}
	return r.store
}

func (r *Registry) watch(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			version, err := r.store.GrokEgressVersion(ctx)
			if err != nil || version == r.version.Load() {
				continue
			}
			if err := r.Reload(ctx); err != nil {
				slog.Warn("egress: reload failed", "error", err)
			}
		}
	}
}

// Reload rebuilds the snapshot from the store.
func (r *Registry) Reload(ctx context.Context) error {
	if r == nil || r.store == nil {
		return nil
	}
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()
	version, err := r.store.GrokEgressVersion(ctx)
	if err != nil {
		return err
	}
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return err
	}
	profiles, err := r.store.ListGrokEgressProxyProfiles(ctx)
	if err != nil {
		return err
	}
	ops, err := r.store.GetGrokEgressOperations(ctx)
	if err != nil {
		return err
	}
	bindings, err := r.store.ListGrokEgressBindings(ctx)
	if err != nil {
		return err
	}
	r.snapshot.Store(buildSnapshot(nodes, profiles, ops, bindings))
	r.version.Store(version)
	return nil
}

func buildSnapshot(nodes []*store.GrokEgressNode, profiles []*store.GrokEgressProxyProfile, ops store.GrokEgressOperations, bindings map[string]string) *Snapshot {
	profileURL := map[string]string{}
	for _, profile := range profiles {
		profileURL[profile.ID] = profile.ProxyURL
	}
	snap := &Snapshot{byID: map[string]Node{}, Invalid: map[string]string{}, Operations: ops, Bindings: map[string]string{}, LoadedAt: time.Now()}
	for _, stored := range nodes {
		if !stored.Enabled {
			continue
		}
		proxy := stored.ProxyURL
		if stored.ProxyProfileID != "" {
			resolved, ok := profileURL[stored.ProxyProfileID]
			if !ok {
				snap.Invalid[stored.ID] = "proxy profile missing"
				continue
			}
			proxy = resolved
		}
		if err := ValidateProxyURL(proxy); err != nil {
			snap.Invalid[stored.ID] = err.Error()
			continue
		}
		if IsTunnelURL(proxy) {
			if err := tunnelRoutable(proxy); err != nil {
				snap.Invalid[stored.ID] = err.Error()
				continue
			}
		}
		node := Node{ID: stored.ID, Name: stored.Name, Scope: stored.Scope, ProxyURL: strings.TrimSpace(proxy), Pool: stored.ProxyPool,
			Capacity: stored.AccountCapacity, UserAgent: strings.TrimSpace(stored.UserAgent), Cookies: strings.TrimSpace(stored.CloudflareCookies), SourceID: stored.SourceID}
		snap.Nodes = append(snap.Nodes, node)
		snap.byID[node.ID] = node
	}
	for field, node := range bindings {
		if _, ok := snap.byID[node]; ok {
			snap.Bindings[field] = node
		}
	}
	return snap
}

// Changed reloads after a local write so the change applies at once rather
// than at the next poll.
func (r *Registry) Changed(ctx context.Context) {
	if err := r.Reload(ctx); err != nil {
		slog.Warn("egress: reload after change failed", "error", err)
	}
}

// migrateConfigNodes moves nodes from the retired grok_egress_nodes setting
// into the registry, once, when the registry is still empty.
func migrateConfigNodes(ctx context.Context, st *store.Store, cfg *config.Config) error {
	if cfg == nil || !cfg.GrokEgressEnabled || len(cfg.GrokEgressNodes) == 0 {
		return nil
	}
	existing, err := st.ListGrokEgressNodes(ctx)
	if err != nil || len(existing) > 0 {
		return err
	}
	now := time.Now()
	var nodes []*store.GrokEgressNode
	for i, legacy := range cfg.GrokEgressNodes {
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
			if name == "" {
				name = fmt.Sprintf("node-%d", i+1)
			}
			nodes = append(nodes, &store.GrokEgressNode{ID: NewID(), Name: name, Scope: scope, Enabled: true, ProxyURL: strings.TrimSpace(legacy.URL), CreatedAt: now, UpdatedAt: now})
		}
	}
	slog.Info("egress: migrated configured nodes", "count", len(nodes))
	return st.SaveGrokEgressNodes(ctx, nodes)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
