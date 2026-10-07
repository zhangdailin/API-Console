package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"orchids-api/internal/httpclient"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/util"
)

// Manager owns the Build proxy pool and per-node health. It is disabled by default (GrokEgressEnabled=false) so existing
// behavior is unchanged until configured. When enabled it is fail-closed: a
// request either gets a valid lease or an error — it never
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
}

var errNoClient = errors.New("egress client not initialized")
var errLeaseReleased = errors.New("egress lease already released")
var errNoHealthyNode = errors.New("egress no healthy node for scope")

// NewManager builds an egress manager from configuration. Returns nil when
// egress is disabled.
func NewManager(cfg *config.Config) *Manager {
	if cfg == nil || !cfg.GrokEgressEnabled {
		return nil
	}
	manager := &Manager{
		cfg:       cfg,
		nodes:     nodesFromConfig(cfg),
		health:    make(map[string]float64),
		unhealthy: make(map[string]time.Time),
		failures:  make(map[string]int),
		lastError: make(map[string]string),
		lastProbe: make(map[string]time.Time),
		usedCount: make(map[string]int),
		sticky:    make(map[string]string),
	}
	manager.restoreHealth()
	return manager
}

// Enabled reports whether the manager is active.
func (m *Manager) Enabled() bool { return m != nil && m.cfg != nil && m.cfg.GrokEgressEnabled }

// Acquire selects a healthy Build proxy node and returns a sticky lease.
// affinity keeps the same account on the same exit. When no healthy node exists,
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

	// Isolate connection pools by node, proxy URL, and affinity binding. The
	// proxy component is hashed so credentials never appear in cache keys or
	// diagnostics, while a same-name node whose URL changes gets a fresh pool.
	poolKey := "egress:" + node.Name + "|proxy=" + shortHash(node.URL) + "|" + fingerprint
	client := httpclient.GetSharedHTTPClient(poolKey, m.cfg.GrokRequestTimeout(strings.ToLower(strings.TrimSpace(scope))), proxyFuncForNode(*node))

	lease := &Lease{
		NodeID:   node.Name,
		ProxyURL: node.URL,
		client:   client,
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
	stickyAffinity = util.FirstNonEmptyUntrimmed(stickyAffinity, "default")
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
	binding := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(node.Name)),
		strings.TrimSpace(node.URL),
		strings.ToLower(strings.TrimSpace(affinity)),
	}, "\x00")
	return shortHash(binding)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}

// FeedbackOutcome updates a node's health score from a classified outcome.
// Success recovers, transport/server failures degrade, and 429/account issues
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
	case OutcomeTransportError, OutcomeServerError:
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
	target := "https://cli-chat-proxy.grok.com/"
	probeCtx, cancel := context.WithTimeout(ctx, nodeProbeTimeout)
	defer cancel()
	client := httpclient.GetSharedHTTPClient("egress-probe:"+node.Name+"|proxy="+shortHash(node.URL), nodeProbeTimeout, proxyFuncForNode(node))
	req, err := http.NewRequestWithContext(probeCtx, http.MethodHead, target, nil)
	if err != nil {
		return err
	}
	// A stable browser UA keeps the probe consistent with real traffic on this
	// exit, so a UA-based block is detected too.
	req.Header.Set("User-Agent", "orchids-2api-build-egress-probe")
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

// egressHealthFileName is the per-deployment health snapshot. It lives in the
// media directory; when replicas mount the same directory, node health survives
// a restart and is visible to every instance.
const egressHealthFileName = ".egress-health.json"

type egressHealthFile struct {
	Nodes []egressHealthEntry `json:"nodes"`
}

type egressHealthEntry struct {
	Name          string  `json:"name"`
	Health        float64 `json:"health"`
	Failures      int     `json:"failures"`
	CooldownUntil string  `json:"cooldown_until,omitempty"`
	LastError     string  `json:"last_error,omitempty"`
}

func (m *Manager) healthFilePath() string {
	if m == nil || m.cfg == nil {
		return ""
	}
	dir := strings.TrimSpace(m.cfg.MediaDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, egressHealthFileName)
}

// restoreHealth loads the persisted snapshot. Only nodes that still exist in
// the configuration are applied, so removing a node also removes its history.
func (m *Manager) restoreHealth() {
	path := m.healthFilePath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snapshot egressHealthFile
	if json.Unmarshal(raw, &snapshot) != nil {
		return
	}
	known := make(map[string]struct{}, len(m.nodes))
	for _, node := range m.nodes {
		known[node.Name] = struct{}{}
	}
	now := time.Now()
	for _, entry := range snapshot.Nodes {
		if _, ok := known[entry.Name]; !ok {
			continue
		}
		m.health[entry.Name] = entry.Health
		m.failures[entry.Name] = entry.Failures
		if entry.LastError != "" {
			m.lastError[entry.Name] = entry.LastError
		}
		if entry.CooldownUntil != "" {
			if until, err := time.Parse(time.RFC3339, entry.CooldownUntil); err == nil && until.After(now) {
				m.unhealthy[entry.Name] = until
			}
		}
	}
}

// persistHealthLocked writes the snapshot, throttled so a burst of failures does
// not turn into a burst of disk writes. Callers must hold m.mu.
func (m *Manager) persistHealthLocked() {
	path := m.healthFilePath()
	if path == "" {
		return
	}
	now := time.Now()
	if !m.lastPersist.IsZero() && now.Sub(m.lastPersist) < egressHealthPersistInterval {
		return
	}
	m.lastPersist = now
	snapshot := egressHealthFile{Nodes: make([]egressHealthEntry, 0, len(m.nodes))}
	for _, node := range m.nodes {
		entry := egressHealthEntry{
			Name:     node.Name,
			Health:   m.health[node.Name],
			Failures: m.failures[node.Name],
		}
		if until, ok := m.unhealthy[node.Name]; ok && until.After(now) {
			entry.CooldownUntil = until.UTC().Format(time.RFC3339)
		}
		if last := m.lastError[node.Name]; last != "" {
			entry.LastError = last
		}
		snapshot.Nodes = append(snapshot.Nodes, entry)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := path + ".tmp-" + shortHash(fmt.Sprint(time.Now().UnixNano()))
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}
