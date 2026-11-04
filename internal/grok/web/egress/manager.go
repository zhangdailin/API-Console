package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/httpclient"
)

// Manager owns the proxy pool, per-node health, Cloudflare clearance cache, and
// UA rotation. It is disabled by default (GrokEgressEnabled=false) so existing
// behavior is unchanged until configured. When enabled it is fail-closed: a
// request either gets a valid lease with clearance, or an error — it never
// silently falls back to a direct client.

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
	// egressHealthPersistInterval throttles snapshot writes.
	egressHealthPersistInterval = 5 * time.Second
)

// FeedbackOutcome categorizes a lease result for node health scoring. It is
// deliberately coarser than upstream classification: the manager only needs to
// know whether a node should be degraded, kept, or recovered.
type FeedbackOutcome int

const (
	OutcomeSuccess FeedbackOutcome = iota
	OutcomeTransportError
	OutcomeServerError
	OutcomeChallenge // persistent Cloudflare/DPoP challenge (node cannot serve)
	OutcomeRateLimited
	OutcomeAccountBlock
	OutcomeForbidden
)

type Manager struct {
	mu        sync.RWMutex
	cfg       *config.Config
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
	clearances  map[string]clearanceState
	lastLease   map[string]leaseKeyInfo // scope|affinity -> last fingerprint/version
	version     uint64                  // clearance generation counter
	solver      clearanceSolver
}

type clearanceState struct {
	cookies     string
	userAgent   string
	refreshedAt time.Time
	invalid     bool
	version     uint64
}

type leaseKeyInfo struct {
	fingerprint string
	version     uint64
	nodeID      string
}

var errNoClient = errors.New("egress client not initialized")
var errNoHealthyNode = errors.New("egress no healthy node for scope")
var errClearanceUnavailable = errors.New("egress clearance unavailable")

// NewManager builds an egress manager from configuration. Returns nil when
// egress is disabled.
func NewManager(cfg *config.Config) *Manager {
	if cfg == nil || !cfg.GrokEgressEnabled {
		return nil
	}
	manager := &Manager{
		cfg:        cfg,
		nodes:      nodesFromConfig(cfg),
		health:     make(map[string]float64),
		unhealthy:  make(map[string]time.Time),
		failures:   make(map[string]int),
		lastError:  make(map[string]string),
		lastProbe:  make(map[string]time.Time),
		usedCount:  make(map[string]int),
		sticky:     make(map[string]string),
		clearances: make(map[string]clearanceState),
		lastLease:  make(map[string]leaseKeyInfo),
		solver:     flaresolverrSolver{},
	}
	manager.restoreHealth()
	return manager
}

// Enabled reports whether the manager is active.
func (m *Manager) Enabled() bool {
	return m != nil && m.cfg != nil && m.cfg.GrokEgressEnabled
}

// Acquire selects a healthy node for a scope, binding a UA + clearance
// fingerprint to it, and returns a lease. affinity (e.g. account identity)
// keeps the same account on the same exit so clearance stays valid. When no
// healthy node exists, or clearance cannot be resolved for a flare-solve node,
// Acquire fails closed.
func (m *Manager) Acquire(ctx context.Context, scope, affinity string) (*Lease, error) {
	if !m.Enabled() {
		return nil, errors.New("egress disabled")
	}
	node := m.pickNode(scope, affinity)
	if node == nil {
		// No healthy node: probe the one closest to the end of its cooldown.
		// The probe runs without the manager lock held (it is a network call),
		// and only a node that answers is admitted again.
		if probed := m.probeRecovery(ctx, scope); probed {
			node = m.pickNode(scope, affinity)
		}
	}
	if node == nil {
		return nil, errNoHealthyNode
	}
	fingerprint := m.fingerprint(*node, affinity)
	var ua, cookies string
	var version uint64
	var err error
	if usesBrowserClearance(scope) {
		ua, cookies, version, err = m.resolveFingerprint(ctx, *node, fingerprint)
		if err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	m.lastLease[scope+"|"+affinity] = leaseKeyInfo{fingerprint: fingerprint, version: version, nodeID: node.Name}
	m.mu.Unlock()

	// Isolate connection pools by node, proxy URL, and clearance binding. The
	// proxy component is hashed so credentials never appear in cache keys or
	// diagnostics, while a same-name node whose URL changes gets a fresh pool.
	poolKey := "egress:" + node.Name + "|proxy=" + shortHash(node.URL) + "|" + fingerprint
	// The ClientHello follows the UA this exit will send, so the TLS fingerprint
	// and the advertised Chrome version cannot contradict each other.
	client := httpclient.GetSharedBrowserHTTPClientForUserAgent(poolKey, m.cfg.GrokWebRequestTimeout(), 0, proxyFuncForNode(*node), ua)

	lease := &Lease{
		NodeID:           node.Name,
		ProxyURL:         node.URL,
		UserAgent:        ua,
		CFCookies:        cookies,
		clearanceKey:     fingerprint,
		clearanceVersion: version,
		client:           client,
		manager:          m,
	}
	return lease, nil
}

func (m *Manager) pickNode(scope, affinity string) *Node {
	m.mu.Lock()
	defer m.mu.Unlock()

	normalizedScope := strings.ToLower(strings.TrimSpace(scope))
	now := time.Now()
	var candidates []Node
	totalWeight := 0
	anyForScope := false
	for _, node := range m.nodes {
		if !nodeMatchesScope(node, normalizedScope) {
			continue
		}
		anyForScope = true
		if m.degradedLocked(node.Name, now) {
			continue
		}
		candidates = append(candidates, node)
		totalWeight += node.Weight
	}
	if len(candidates) == 0 {
		// Every node for this scope is cooling down. Rather than waiting out the
		// whole window (or failing instantly), probe the node whose cooldown ends
		// first and admit it when it answers: recovery is bounded by the probe,
		// and a node that is really down keeps its (now longer) cooldown.
		if anyForScope {
			recordAllNodesUnhealthy(normalizedScope)
		}
		return nil
	}

	// Sticky affinity: prefer the node this affinity last used when healthy.
	stickyAffinity := affinity
	if stickyAffinity == "" {
		stickyAffinity = "default"
	}
	stickyKey := "scope:" + normalizedScope + ":" + stickyAffinity
	if prev := m.sticky[stickyKey]; prev != "" {
		for i := range candidates {
			if candidates[i].Name == prev {
				recordNodeSelection(normalizedScope)
				return &candidates[i]
			}
		}
	}

	// Weighted round-robin over healthy candidates.
	seed := m.usedCount[stickyKey]
	m.usedCount[stickyKey] = seed + 1
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
	m.sticky[stickyKey] = pick.Name
	recordNodeSelection(normalizedScope)
	return pick
}

// degradedLocked reports whether a node is in its failure cooldown. Callers must
// hold m.mu. A zero health score means the node has never been probed and is
// usable; only scores pushed below the skip threshold by FeedbackOutcome trip
// the cooldown.
func (m *Manager) degradedLocked(name string, now time.Time) bool {
	score := m.health[name]
	if score >= healthSkipThreshold || score == 0 {
		return false
	}
	return now.Before(m.unhealthy[name])
}

func (m *Manager) fingerprint(node Node, affinity string) string {
	cfg := m.clearanceConfig()
	binding := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(node.Name)),
		strings.TrimSpace(node.URL),
		strings.TrimRight(strings.TrimSpace(cfg.FlareSolverrURL), "/"),
		strings.TrimRight(strings.TrimSpace(cfg.TargetURL), "/"),
		strings.ToLower(strings.TrimSpace(affinity)),
	}, "\x00")
	return shortHash(binding)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

func usesBrowserClearance(scope string) bool {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "cli", "build", "console_asset":
		return false
	default:
		return true
	}
}

// resolveFingerprint returns a stable (ua, cookies, version) pair for a
// fingerprint, solving/refreshing clearance when needed. A failed solve with no
// reusable cached clearance fails closed (error) so the caller never proceeds
// with empty cookies pretending to be valid.
func (m *Manager) resolveFingerprint(ctx context.Context, node Node, fingerprint string) (string, string, uint64, error) {
	m.mu.Lock()
	state, ok := m.clearances[fingerprint]
	m.mu.Unlock()

	if ok && !state.invalid && state.cookies != "" && time.Since(state.refreshedAt) < m.refreshInterval() {
		return state.userAgent, state.cookies, state.version, nil
	}

	cfg := m.clearanceConfig()
	ua := state.userAgent
	if ua == "" {
		ua = pickUserAgent(fingerprint)
	}
	cookies := state.cookies

	if cfg.Mode == "flaresolverr" && m.solver != nil {
		solved, err := m.solver.Solve(ctx, cfg, node.URL)
		if err != nil {
			// Reuse a stale-but-not-invalidated clearance rather than failing the
			// request outright, but never write a "fresh" success cache entry.
			if ok && !state.invalid && state.cookies != "" {
				slog.Warn("egress clearance solve failed; reusing stale clearance", "node", node.Name, "error", err)
				recordClearanceSolve(false)
				return state.userAgent, state.cookies, state.version, nil
			}
			recordClearanceSolve(false)
			return "", "", 0, fmt.Errorf("%w: solve for node %q: %w", errClearanceUnavailable, node.Name, err)
		}
		if solved.UserAgent != "" {
			ua = solved.UserAgent
		}
		if strings.TrimSpace(solved.Cookies) == "" {
			// Empty cookies after a "successful" solve is not a valid clearance.
			if ok && !state.invalid && state.cookies != "" {
				slog.Warn("egress clearance solve returned no cookies; reusing stale clearance", "node", node.Name)
				recordClearanceSolve(false)
				return state.userAgent, state.cookies, state.version, nil
			}
			recordClearanceSolve(false)
			return "", "", 0, fmt.Errorf("%w: solve returned empty cookies for node %q", errClearanceUnavailable, node.Name)
		}
		cookies = solved.Cookies
		recordClearanceSolve(true)
	} else if cfg.Mode != "flaresolverr" && cookies == "" {
		// Manual mode: pull from static config when present. Empty is acceptable
		// here — the operator explicitly chose manual without clearance.
		cookies = m.manualClearance()
	}

	m.mu.Lock()
	m.version++
	state = clearanceState{
		cookies:     cookies,
		userAgent:   ua,
		refreshedAt: time.Now(),
		version:     m.version,
	}
	m.clearances[fingerprint] = state
	m.mu.Unlock()
	return ua, cookies, state.version, nil
}

func (m *Manager) manualClearance() string {
	if m == nil || m.cfg == nil {
		return ""
	}
	parts := make([]string, 0, 2)
	if v := strings.TrimSpace(m.cfg.GrokConfigCFClearance); v != "" {
		parts = append(parts, "cf_clearance="+v)
	}
	if v := strings.TrimSpace(m.cfg.GrokConfigCFBM); v != "" {
		parts = append(parts, "__cf_bm="+v)
	}
	return strings.Join(parts, "; ")
}

func (m *Manager) clearanceConfig() ClearanceConfig {
	if m == nil || m.cfg == nil {
		return ClearanceConfig{Mode: "manual", TargetURL: "https://grok.com"}
	}
	return ClearanceConfig{
		Mode:            m.cfg.GrokClearanceModeOrDefault(),
		FlareSolverrURL: strings.TrimSpace(m.cfg.GrokFlareSolverrURL),
		TargetURL:       "https://grok.com",
		Timeout:         time.Minute,
	}
}

func (m *Manager) refreshInterval() time.Duration {
	if m == nil || m.cfg == nil {
		return 10 * time.Minute
	}
	return time.Duration(m.cfg.GrokClearanceRefreshIntervalOrDefault()) * time.Second
}

// FeedbackOutcome updates a node's health score from a classified outcome.
// Success recovers, transport/server/challenge degrade, and 429/account issues
// leave the node untouched (they are not the node's fault).
func (m *Manager) FeedbackOutcome(nodeID string, outcome FeedbackOutcome) {
	if m == nil || nodeID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	score := m.health[nodeID]
	wasDegraded := score < healthSkipThreshold && score != 0

	switch outcome {
	case OutcomeSuccess:
		newScore := score + (1.0-score)*0.1
		m.health[nodeID] = newScore
		// A working node starts over: the next failure costs the base cooldown,
		// not the accumulated one.
		m.failures[nodeID] = 0
		delete(m.lastError, nodeID)
		if wasDegraded {
			delete(m.unhealthy, nodeID)
			recordNodeRecovery(m.scopeForNodeLocked(nodeID))
		}
	case OutcomeTransportError, OutcomeServerError, OutcomeChallenge:
		// Failures push below zero so a fresh node (score 0) degrades on the
		// first failure, and an exponential cooldown window lets it retry.
		m.health[nodeID] = score*0.5 - 0.1
		m.failures[nodeID]++
		m.lastError[nodeID] = outcomeReason(outcome)
		m.unhealthy[nodeID] = time.Now().Add(nodeCooldownFor(m.failures[nodeID]))
		// Count the failure itself, not only a repeat failure of an already
		// degraded node: the healthy->degraded transition is the event an
		// operator alert is about.
		recordNodeFailure(m.scopeForNodeLocked(nodeID), outcomeReason(outcome))
	case OutcomeRateLimited, OutcomeAccountBlock, OutcomeForbidden:
		// No health change: the request failed for account/team-level reasons.
	}
	m.persistHealthLocked()
}

func (m *Manager) scopeForNodeLocked(nodeID string) string {
	for _, node := range m.nodes {
		if node.Name == nodeID {
			return strings.ToLower(strings.TrimSpace(node.Scope))
		}
	}
	return "all"
}

func outcomeReason(outcome FeedbackOutcome) string {
	switch outcome {
	case OutcomeServerError:
		return "server"
	case OutcomeChallenge:
		return "challenge"
	default:
		return "transport"
	}
}

// InvalidateAffinityClearance invalidates the clearance used by the last lease
// for a scope+affinity. Used by request paths that do not hold the lease
// directly (e.g. CLI) after classifying a Cloudflare challenge.
func (m *Manager) InvalidateAffinityClearance(scope, affinity string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	info, ok := m.lastLease[scope+"|"+affinity]
	m.mu.RUnlock()
	if !ok || info.fingerprint == "" {
		return
	}
	m.invalidateClearanceKey(info.fingerprint, info.version)
}

// FeedbackAffinityOutcome applies a node-health outcome to the node most
// recently leased for a scope+affinity. Used by request paths that do not hold
// the lease directly (e.g. CLI) to record a persistent challenge after retry.
func (m *Manager) FeedbackAffinityOutcome(scope, affinity string, outcome FeedbackOutcome) {
	if m == nil {
		return
	}
	m.mu.RLock()
	info, ok := m.lastLease[scope+"|"+affinity]
	m.mu.RUnlock()
	if !ok || info.nodeID == "" {
		return
	}
	m.FeedbackOutcome(info.nodeID, outcome)
}

// invalidateClearanceKey marks a fingerprint's clearance stale. When version is
// non-zero it only applies to that exact generation, so a concurrent re-solve
// that bumped the version is never invalidated by a stale call.
func (m *Manager) invalidateClearanceKey(key string, version uint64) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.clearances[key]
	if !ok {
		return
	}
	if version != 0 && state.version != version {
		return
	}
	state.invalid = true
	m.clearances[key] = state
	recordClearanceInvalidation()
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
// probe is a network request and must not stall every other Acquire; phase
// three updates health under the lock again.
func (m *Manager) probeRecovery(ctx context.Context, scope string) bool {
	normalizedScope := strings.ToLower(strings.TrimSpace(scope))
	now := time.Now()

	m.mu.Lock()
	var candidate *Node
	var earliest time.Time
	for i := range m.nodes {
		node := m.nodes[i]
		if !nodeMatchesScope(node, normalizedScope) {
			continue
		}
		until, cooling := m.unhealthy[node.Name]
		if !cooling {
			continue
		}
		if last, probed := m.lastProbe[node.Name]; probed && now.Sub(last) < nodeCooldown {
			continue
		}
		if candidate == nil || until.Before(earliest) {
			candidate = &m.nodes[i]
			earliest = until
		}
	}
	if candidate == nil {
		m.mu.Unlock()
		return false
	}
	node := *candidate
	m.lastProbe[node.Name] = now
	m.mu.Unlock()

	if err := m.probeNode(ctx, node); err != nil {
		m.mu.Lock()
		m.failures[node.Name]++
		m.lastError[node.Name] = "probe"
		m.unhealthy[node.Name] = time.Now().Add(nodeCooldownFor(m.failures[node.Name]))
		m.mu.Unlock()
		return false
	}

	m.mu.Lock()
	delete(m.unhealthy, node.Name)
	m.health[node.Name] = healthSkipThreshold
	m.failures[node.Name] = 0
	delete(m.lastError, node.Name)
	m.persistHealthLocked()
	m.mu.Unlock()
	recordNodeRecovery(normalizedScope)
	return true
}

// probeNode issues one short request through the node to see whether it can
// reach the upstream at all. It deliberately goes through the same proxy path a
// real request would, so a broken exit is detected rather than a healthy direct
// connection.
func (m *Manager) probeNode(ctx context.Context, node Node) error {
	cfg := m.clearanceConfig()
	target := strings.TrimSpace(cfg.TargetURL)
	if target == "" {
		target = "https://grok.com/"
	}
	probeCtx, cancel := context.WithTimeout(ctx, nodeProbeTimeout)
	defer cancel()
	client := httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(
		"egress-probe:"+node.Name+"|proxy="+shortHash(node.URL),
		nodeProbeTimeout, 0, proxyFuncForNode(node))
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	// A stable browser UA keeps the probe consistent with real traffic on this
	// exit, so a UA-based block is detected too.
	req.Header.Set("User-Agent", pickUserAgent(node.Name))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// A Cloudflare challenge means the exit is reachable but not usable; the
	// caller will re-solve clearance on the next Acquire.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
		return fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return nil
}
