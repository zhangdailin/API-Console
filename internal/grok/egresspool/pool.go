// Package egresspool is the proxy node pool shared by the Grok Build and Grok
// Web egress managers: node selection, health scoring, failure cooldowns,
// recovery probes and health persistence.
//
// What differs between the planes stays with each plane's egress package: how
// a lease and its HTTP client are built, how a node is probed, and (Web only)
// Cloudflare clearance. A Profile carries the per-plane values the pool needs.
//
// The pool is fail-closed: Pick returns a node, a direct fallback the scope's
// configuration asked for, or an error. It never silently goes direct.
package egresspool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/egressnodes"
)

const (
	healthSkipThreshold = 0.2
	// nodeCooldown is the first failure cooldown. Each further consecutive
	// failure doubles it up to nodeCooldownMax, so a node that is genuinely
	// down stops being retried every 30 seconds while a transient wobble still
	// recovers quickly.
	nodeCooldown    = 30 * time.Second
	nodeCooldownMax = 10 * time.Minute
	// nodeProbeTimeout bounds the active probe issued when every node for a
	// scope is cooling down.
	nodeProbeTimeout = 6 * time.Second
	// healthPersistInterval throttles snapshot writes.
	healthPersistInterval = 5 * time.Second
)

// ErrNoHealthyNode is returned when a scope has no usable node and its
// fallback does not allow going direct.
var ErrNoHealthyNode = errors.New("egress no healthy node for scope")

// Outcome categorizes a lease result for node health scoring. It is
// deliberately coarser than upstream classification: the pool only needs to
// know whether a node should be degraded, kept, or recovered.
type Outcome int

const (
	OutcomeSuccess Outcome = iota
	OutcomeTransportError
	OutcomeServerError
	OutcomeChallenge // persistent Cloudflare/DPoP challenge (node cannot serve)
	OutcomeRateLimited
	OutcomeAccountBlock
	OutcomeForbidden
)

// Profile holds what differs between the planes sharing the pool.
type Profile struct {
	// Namespace is the Prometheus namespace of the pool's metrics.
	Namespace string
	// HealthFile is the snapshot file name inside the media directory.
	HealthFile string
	// Probe checks whether a node can reach the upstream. The context already
	// carries the probe timeout.
	Probe func(ctx context.Context, cfg *config.Config, node Node) error
}

// Pool owns one plane's node list and per-node health.
type Pool struct {
	mu        sync.RWMutex
	cfg       *config.Config
	profile   Profile
	metrics   *metrics
	nodes     []Node
	health    map[string]float64
	unhealthy map[string]time.Time // node -> cooldown expiry after degradation
	failures  map[string]int       // node -> consecutive failures (drives the cooldown)
	lastError map[string]string    // node -> last classified failure (diagnostics)
	lastProbe map[string]time.Time // node -> last active probe attempt
	// lastPersist throttles health-snapshot writes.
	lastPersist time.Time
	usedCount   map[string]int
	sticky      map[string]string // scope|affinity -> node name
	snap        *egressnodes.Snapshot
}

// New builds a pool from configuration and restores persisted health.
func New(cfg *config.Config, profile Profile) *Pool {
	p := &Pool{
		cfg:       cfg,
		profile:   profile,
		metrics:   metricsFor(profile.Namespace),
		health:    make(map[string]float64),
		unhealthy: make(map[string]time.Time),
		failures:  make(map[string]int),
		lastError: make(map[string]string),
		lastProbe: make(map[string]time.Time),
		usedCount: make(map[string]int),
		sticky:    make(map[string]string),
	}
	p.mu.Lock()
	p.refreshNodesLocked()
	p.mu.Unlock()
	p.restoreHealth()
	return p
}

// SetConfig swaps the configuration; health, cooldowns and sticky routing are
// kept.
func (p *Pool) SetConfig(cfg *config.Config) {
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
}

// Config returns the current configuration.
func (p *Pool) Config() *config.Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// refreshNodesLocked converts the current registry snapshot. Callers hold p.mu.
func (p *Pool) refreshNodesLocked() {
	snap := egressnodes.SnapshotFor(p.cfg)
	if snap == p.snap && p.nodes != nil {
		return
	}
	p.snap = snap
	nodes := make([]Node, 0, len(snap.Nodes))
	for _, node := range snap.Nodes {
		nodes = append(nodes, nodeFromRegistry(node))
	}
	p.nodes = nodes
}

func (p *Pool) snapshot() *egressnodes.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshNodesLocked()
	return p.snap
}

// Node returns the current registry node with that ID.
func (p *Pool) Node(id string) (Node, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshNodesLocked()
	for _, node := range p.nodes {
		if node.Name == id {
			return node, true
		}
	}
	return Node{}, false
}

// Nodes returns a copy of the current node list.
func (p *Pool) Nodes() []Node {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshNodesLocked()
	return append([]Node(nil), p.nodes...)
}

// Routes reports whether requests for scope route through the pool: the scope
// has nodes, or its fallback is not the general proxy.
func (p *Pool) Routes(scope string) bool {
	if p == nil {
		return false
	}
	return p.snapshot().Routes(egressnodes.RegistryScope(scope))
}

// targetScopeLocked maps a request scope to the node scope serving it: Web
// asset downloads use asset nodes when any exist, Web nodes otherwise.
func (p *Pool) targetScopeLocked(scope string) string {
	target := egressnodes.RegistryScope(scope)
	if target == "grok_web_asset" {
		for _, node := range p.nodes {
			if node.Scope == target {
				return target
			}
		}
		return "grok_web"
	}
	return target
}

// Pick selects the node for a scope and affinity. When no node is healthy it
// probes the one closest to the end of its cooldown, then applies the scope's
// fallback. direct reports a fallback to the general proxy; err means the
// request must fail closed.
func (p *Pool) Pick(ctx context.Context, scope, affinity string) (node *Node, direct bool, err error) {
	if p == nil {
		return nil, false, errors.New("egress disabled")
	}
	node = p.pickNode(scope, affinity)
	if node == nil {
		// The probe runs without the pool lock held (it is a network call), and
		// only a node that answers is admitted again.
		if p.probeRecovery(ctx, scope) {
			node = p.pickNode(scope, affinity)
		}
	}
	if node == nil {
		fallback := p.snapshot().Fallback(egressnodes.RegistryScope(scope))
		switch fallback.Mode {
		case "direct":
			return nil, true, nil
		case "fixed":
			if fixed, ok := p.snapshot().Node(fallback.NodeID); ok {
				converted := nodeFromRegistry(fixed)
				node = &converted
			}
		}
	}
	if node == nil {
		return nil, false, ErrNoHealthyNode
	}
	return node, false, nil
}

func (p *Pool) pickNode(scope, affinity string) *Node {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.refreshNodesLocked()
	normalizedScope := strings.ToLower(strings.TrimSpace(scope))
	target := p.targetScopeLocked(normalizedScope)
	now := time.Now()
	var candidates []Node
	totalWeight := 0
	anyForScope := false
	for _, node := range p.nodes {
		if node.Scope != target {
			continue
		}
		anyForScope = true
		if p.degradedLocked(node.Name, now) {
			continue
		}
		candidates = append(candidates, node)
		totalWeight += node.Weight
	}
	if len(candidates) == 0 {
		// Every node for this scope is cooling down. Rather than waiting out the
		// whole window (or failing instantly), Pick probes the node whose cooldown
		// ends first and admits it when it answers: recovery is bounded by the
		// probe, and a node that is really down keeps its (now longer) cooldown.
		if anyForScope {
			p.metrics.allNodesUnhealthy.WithLabelValues(normalizedScope).Inc()
		}
		return nil
	}

	// An account bound to a node keeps it while it is healthy.
	bound := p.snap.Binding(egressnodes.RegistryScope(normalizedScope), affinity)
	if bound == "" {
		ids := make([]string, 0, len(candidates))
		for _, candidate := range candidates {
			ids = append(ids, candidate.Name)
		}
		bound = egressnodes.AutoBind(egressnodes.RegistryScope(normalizedScope), affinity, ids)
	}
	if bound != "" {
		for i := range candidates {
			if candidates[i].Name == bound {
				p.metrics.selections.WithLabelValues(normalizedScope).Inc()
				return &candidates[i]
			}
		}
	}

	// Sticky affinity: prefer the node this affinity last used when healthy.
	stickyAffinity := affinity
	if stickyAffinity == "" {
		stickyAffinity = "default"
	}
	stickyKey := "scope:" + normalizedScope + ":" + stickyAffinity
	if prev := p.sticky[stickyKey]; prev != "" {
		for i := range candidates {
			if candidates[i].Name == prev {
				p.metrics.selections.WithLabelValues(normalizedScope).Inc()
				return &candidates[i]
			}
		}
	}

	// Weighted round-robin over healthy candidates.
	seed := p.usedCount[stickyKey]
	p.usedCount[stickyKey] = seed + 1
	threshold := (seed + 1) % totalWeight
	var pick *Node
	for i := range candidates {
		threshold -= candidates[i].Weight
		if threshold < 0 {
			pick = &candidates[i]
			break
		}
	}
	if pick == nil {
		pick = &candidates[len(candidates)-1]
	}
	p.sticky[stickyKey] = pick.Name
	p.metrics.selections.WithLabelValues(normalizedScope).Inc()
	return pick
}

// degradedLocked reports whether a node is in its failure cooldown. Callers must
// hold p.mu. A zero health score means the node has never been probed and is
// usable; only scores pushed below the skip threshold by Feedback trip the
// cooldown.
func (p *Pool) degradedLocked(name string, now time.Time) bool {
	score := p.health[name]
	if score >= healthSkipThreshold || score == 0 {
		return false
	}
	return now.Before(p.unhealthy[name])
}

// ShortHash is a short, non-reversible digest used in client pool keys, so
// proxy credentials never appear in cache keys or diagnostics.
func ShortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

// Feedback updates a node's health score from a classified outcome. Success
// recovers, transport/server/challenge failures degrade, and 429/account issues
// leave the node untouched (they are not the node's fault).
func (p *Pool) Feedback(nodeID string, outcome Outcome) {
	if p == nil || nodeID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	score := p.health[nodeID]
	wasDegraded := score < healthSkipThreshold && score != 0

	switch outcome {
	case OutcomeSuccess:
		p.health[nodeID] = score + (1.0-score)*0.1
		// A working node starts over: the next failure costs the base cooldown,
		// not the accumulated one.
		p.failures[nodeID] = 0
		delete(p.lastError, nodeID)
		if wasDegraded {
			delete(p.unhealthy, nodeID)
			p.metrics.recoveries.WithLabelValues(p.scopeForNodeLocked(nodeID)).Inc()
		}
	case OutcomeTransportError, OutcomeServerError, OutcomeChallenge:
		// Failures push below zero so a fresh node (score 0) degrades on the
		// first failure, and an exponential cooldown window lets it retry.
		p.health[nodeID] = score*0.5 - 0.1
		p.failures[nodeID]++
		p.lastError[nodeID] = outcomeReason(outcome)
		p.unhealthy[nodeID] = time.Now().Add(nodeCooldownFor(p.failures[nodeID]))
		// Count the failure itself, not only a repeat failure of an already
		// degraded node: the healthy->degraded transition is the event an
		// operator alert is about.
		p.metrics.failures.WithLabelValues(p.scopeForNodeLocked(nodeID), outcomeReason(outcome)).Inc()
	case OutcomeRateLimited, OutcomeAccountBlock, OutcomeForbidden:
		// No health change: the request failed for account/team-level reasons.
	}
	p.reportHealthLocked(nodeID)
	p.persistHealthLocked()
}

func (p *Pool) reportHealthLocked(nodeID string) {
	egressnodes.ReportHealth(nodeID, egressnodes.Health{Score: p.health[nodeID], Failures: p.failures[nodeID], CooldownUntil: p.unhealthy[nodeID], LastError: p.lastError[nodeID]})
}

func (p *Pool) scopeForNodeLocked(nodeID string) string {
	for _, node := range p.nodes {
		if node.Name == nodeID {
			return strings.ToLower(strings.TrimSpace(node.Scope))
		}
	}
	return "all"
}

func outcomeReason(outcome Outcome) string {
	switch outcome {
	case OutcomeServerError:
		return "server"
	case OutcomeChallenge:
		return "challenge"
	default:
		return "transport"
	}
}

// nodeCooldownFor returns the cooldown for a node that has failed n times in a
// row: 30s, 1m, 2m, … capped at 10 minutes.
func nodeCooldownFor(failures int) time.Duration {
	if failures <= 1 {
		return nodeCooldown
	}
	cooldown := nodeCooldown
	for i := 1; i < failures; i++ {
		cooldown *= 2
		if cooldown >= nodeCooldownMax {
			return nodeCooldownMax
		}
	}
	return cooldown
}

// probeRecovery tries to bring one cooling-down node back early.
//
// Phase one (under the lock) picks the node whose cooldown ends first and
// records the attempt; phase two dials it without holding the lock, because a
// probe is a network request and must not stall every other Pick; phase three
// updates health under the lock again.
func (p *Pool) probeRecovery(ctx context.Context, scope string) bool {
	normalizedScope := strings.ToLower(strings.TrimSpace(scope))
	now := time.Now()

	p.mu.Lock()
	p.refreshNodesLocked()
	target := p.targetScopeLocked(normalizedScope)
	cfg := p.cfg
	var candidate *Node
	var earliest time.Time
	for i := range p.nodes {
		node := p.nodes[i]
		if node.Scope != target {
			continue
		}
		until, cooling := p.unhealthy[node.Name]
		if !cooling {
			continue
		}
		if last, probed := p.lastProbe[node.Name]; probed && now.Sub(last) < nodeCooldown {
			continue
		}
		if candidate == nil || until.Before(earliest) {
			candidate = &p.nodes[i]
			earliest = until
		}
	}
	if candidate == nil {
		p.mu.Unlock()
		return false
	}
	node := *candidate
	p.lastProbe[node.Name] = now
	p.mu.Unlock()

	if err := p.probe(ctx, cfg, node); err != nil {
		p.mu.Lock()
		p.failures[node.Name]++
		p.lastError[node.Name] = "probe"
		p.unhealthy[node.Name] = time.Now().Add(nodeCooldownFor(p.failures[node.Name]))
		p.reportHealthLocked(node.Name)
		p.mu.Unlock()
		return false
	}

	p.mu.Lock()
	delete(p.unhealthy, node.Name)
	p.health[node.Name] = healthSkipThreshold
	p.failures[node.Name] = 0
	delete(p.lastError, node.Name)
	p.reportHealthLocked(node.Name)
	p.persistHealthLocked()
	p.mu.Unlock()
	p.metrics.recoveries.WithLabelValues(normalizedScope).Inc()
	return true
}

func (p *Pool) probe(ctx context.Context, cfg *config.Config, node Node) error {
	if p.profile.Probe == nil {
		return errors.New("no probe configured")
	}
	probeCtx, cancel := context.WithTimeout(ctx, nodeProbeTimeout)
	defer cancel()
	return p.profile.Probe(probeCtx, cfg, node)
}

// ProbeTimeout bounds a probe's HTTP client.
const ProbeTimeout = nodeProbeTimeout

// ProbeThrough issues one HEAD request through a node's client to see whether
// it can reach the upstream at all. It deliberately goes through the same proxy
// path a real request would, so a broken exit is detected rather than a healthy
// direct connection. A Cloudflare challenge (403) or a server error means the
// exit is reachable but not usable.
func ProbeThrough(ctx context.Context, client *http.Client, target, userAgent string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return nil
}
