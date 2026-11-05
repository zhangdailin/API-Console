package egresspool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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

// healthFilePath is the plane's health snapshot. It lives in the media
// directory, which a multi-replica deployment already shares (shared_media), so
// node health survives a restart and is visible to every instance that mounts
// the same directory.
func (p *Pool) healthFilePath() string {
	if p == nil || p.cfg == nil {
		return ""
	}
	dir := strings.TrimSpace(p.cfg.MediaDir)
	if dir == "" || p.profile.HealthFile == "" {
		return ""
	}
	return filepath.Join(dir, p.profile.HealthFile)
}

// restoreHealth loads the persisted snapshot. Only nodes that still exist in
// the configuration are applied, so removing a node also removes its history.
func (p *Pool) restoreHealth() {
	path := p.healthFilePath()
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
	known := make(map[string]struct{}, len(p.nodes))
	for _, node := range p.nodes {
		known[node.Name] = struct{}{}
	}
	now := time.Now()
	for _, entry := range snapshot.Nodes {
		if _, ok := known[entry.Name]; !ok {
			continue
		}
		p.health[entry.Name] = entry.Health
		p.failures[entry.Name] = entry.Failures
		if entry.LastError != "" {
			p.lastError[entry.Name] = entry.LastError
		}
		if entry.CooldownUntil != "" {
			if until, err := time.Parse(time.RFC3339, entry.CooldownUntil); err == nil && until.After(now) {
				p.unhealthy[entry.Name] = until
			}
		}
	}
}

// persistHealthLocked writes the snapshot, throttled so a burst of failures does
// not turn into a burst of disk writes. Callers must hold p.mu.
func (p *Pool) persistHealthLocked() {
	path := p.healthFilePath()
	if path == "" {
		return
	}
	now := time.Now()
	if !p.lastPersist.IsZero() && now.Sub(p.lastPersist) < healthPersistInterval {
		return
	}
	p.lastPersist = now
	snapshot := egressHealthFile{Nodes: make([]egressHealthEntry, 0, len(p.nodes))}
	for _, node := range p.nodes {
		entry := egressHealthEntry{
			Name:     node.Name,
			Health:   p.health[node.Name],
			Failures: p.failures[node.Name],
		}
		if until, ok := p.unhealthy[node.Name]; ok && until.After(now) {
			entry.CooldownUntil = until.UTC().Format(time.RFC3339)
		}
		if last := p.lastError[node.Name]; last != "" {
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
	tmp := path + ".tmp-" + ShortHash(fmt.Sprint(time.Now().UnixNano()))
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}
