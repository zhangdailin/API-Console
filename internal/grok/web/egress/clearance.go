package egress

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"orchids-api/internal/grok/egressnodes"
	"strings"
	"time"
)

// resolveFingerprint returns a stable (ua, cookies, version) pair for a
// fingerprint, solving/refreshing clearance when needed. A failed solve with no
// reusable cached clearance fails closed (error) so the caller never proceeds
// with empty cookies pretending to be valid.
func (m *Manager) resolveFingerprint(ctx context.Context, node Node, fingerprint string) (string, string, uint64, error) {
	cfg := m.clearanceConfig()
	// Manual mode with cookies on the node: they were issued to the node's UA
	// through its exit, so both are used exactly as entered.
	if cfg.Mode == "manual" && node.Cookies != "" {
		ua := node.UserAgent
		if ua == "" {
			ua = pickUserAgent(fingerprint)
		}
		m.mu.Lock()
		state, ok := m.clearances[fingerprint]
		if !ok || state.cookies != node.Cookies || state.userAgent != ua {
			m.version++
			state = clearanceState{nodeID: node.Name, cookies: node.Cookies, userAgent: ua, refreshedAt: time.Now(), version: m.version}
			m.clearances[fingerprint] = state
		}
		m.mu.Unlock()
		return state.userAgent, state.cookies, state.version, nil
	}

	m.mu.Lock()
	state, ok := m.clearances[fingerprint]
	m.mu.Unlock()

	fresh := time.Since(state.refreshedAt) < m.refreshInterval()
	if cfg.Mode == "on_demand" {
		// On demand keeps the last good clearance until upstream rejects it.
		fresh = true
	}
	if ok && !state.invalid && state.cookies != "" && fresh {
		return state.userAgent, state.cookies, state.version, nil
	}

	ua := state.userAgent
	if ua == "" {
		ua = pickUserAgent(fingerprint)
	}
	cookies := state.cookies

	if cfg.Mode == "manual" && node.UserAgent != "" {
		ua = node.UserAgent
	}
	if (cfg.Mode == "flaresolverr" || cfg.Mode == "on_demand") && m.solver != nil {
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
	} else if cfg.Mode == "manual" && cookies == "" {
		// Manual mode: pull from static config when present. Empty is acceptable
		// here — the operator explicitly chose manual without clearance.
		cookies = m.manualClearance()
	}

	m.mu.Lock()
	m.version++
	state = clearanceState{
		nodeID:      node.Name,
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
		Timeout:         m.cfg.GrokClearanceTimeoutOrDefault(),
	}
}

func (m *Manager) refreshInterval() time.Duration {
	if m == nil || m.cfg == nil {
		return 10 * time.Minute
	}
	return time.Duration(m.cfg.GrokClearanceRefreshIntervalOrDefault()) * time.Second
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

// InspectClearance reports the freshest clearance a node holds.
func (m *Manager) InspectClearance(nodeID string) egressnodes.ClearanceInfo {
	cfg := m.clearanceConfig()
	info := egressnodes.ClearanceInfo{Mode: cfg.Mode, Status: "missing"}
	m.mu.Lock()
	m.refreshNodesLocked()
	var node *Node
	for i := range m.nodes {
		if m.nodes[i].Name == nodeID {
			node = &m.nodes[i]
		}
	}
	for _, state := range m.clearances {
		if state.nodeID != nodeID || state.refreshedAt.Before(info.RefreshedAt) {
			continue
		}
		info.RefreshedAt = state.refreshedAt
		switch {
		case state.invalid:
			info.Status = "invalid"
		case cfg.Mode == "flaresolverr" && time.Since(state.refreshedAt) >= m.refreshInterval():
			info.Status = "stale"
		default:
			info.Status = "ready"
		}
	}
	m.mu.Unlock()
	if cfg.Mode == "manual" {
		info.Status = "manual"
		if node != nil && node.Cookies == "" && m.manualClearance() == "" {
			info.Status = "missing"
		}
	}
	return info
}

// RefreshClearance solves a fresh clearance for a node through its exit.
func (m *Manager) RefreshClearance(ctx context.Context, nodeID string) error {
	cfg := m.clearanceConfig()
	if cfg.Mode == "manual" {
		return errors.New("clearance is maintained manually")
	}
	m.mu.Lock()
	m.refreshNodesLocked()
	var node *Node
	for i := range m.nodes {
		if m.nodes[i].Name == nodeID {
			copied := m.nodes[i]
			node = &copied
		}
	}
	for key, state := range m.clearances {
		if state.nodeID == nodeID {
			state.invalid = true
			m.clearances[key] = state
		}
	}
	m.mu.Unlock()
	if node == nil {
		return errors.New("node is not routable")
	}
	_, _, _, err := m.resolveFingerprint(ctx, *node, m.fingerprint(*node, ""))
	return err
}
