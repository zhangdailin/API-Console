package egress

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HealthSnapshot reports per-node health for the admin surface. It never
// includes proxy URLs or credentials: only the node name, score, failure count
// and the cooldown deadline.
func (m *Manager) HealthSnapshot() []map[string]interface{} {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := time.Now()
	out := make([]map[string]interface{}, 0, len(m.nodes))
	for _, node := range m.nodes {
		entry := map[string]interface{}{
			"name":     node.Name,
			"scope":    strings.ToLower(strings.TrimSpace(node.Scope)),
			"health":   m.health[node.Name],
			"failures": m.failures[node.Name],
			"healthy":  !m.degradedLocked(node.Name, now),
		}
		if until, ok := m.unhealthy[node.Name]; ok && until.After(now) {
			entry["cooldown_until"] = until.UTC().Format(time.RFC3339)
		}
		if last := m.lastError[node.Name]; last != "" {
			entry["last_error"] = last
		}
		out = append(out, entry)
	}
	return out
}

// egressHealthFileName is the per-deployment health snapshot. It lives in the
// media directory, which a multi-replica deployment already shares
// (shared_media), so node health survives a restart and is visible to every
// instance that mounts the same directory.
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
