package egressnodes

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/store"
)

// ── proxy profiles ────────────────────────────────────────────────────────

// ProfileDTO is a proxy profile as the admin page sees it.
type ProfileDTO struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	ProxyDisplay     string    `json:"proxy_display"`
	ProxyFingerprint string    `json:"proxy_fingerprint"`
	BoundNodeCount   int       `json:"bound_node_count"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type profileInput struct {
	Name     string `json:"name"`
	ProxyURL string `json:"proxy_url"`
}

func (r *Registry) serveProfiles(ctx context.Context, req *http.Request, id string) (any, error) {
	switch {
	case id == "" && req.Method == http.MethodGet:
		return r.listProfiles(ctx)
	case id == "" && req.Method == http.MethodPost:
		return r.saveProfile(ctx, "", req)
	case id != "" && req.Method == http.MethodPut:
		return r.saveProfile(ctx, id, req)
	case id != "" && req.Method == http.MethodDelete:
		return r.deleteProfile(ctx, id)
	}
	return nil, errNotFound
}

func (r *Registry) listProfiles(ctx context.Context) ([]ProfileDTO, error) {
	profiles, err := r.store.ListGrokEgressProxyProfiles(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	bound := map[string]int{}
	for _, node := range nodes {
		bound[node.ProxyProfileID]++
	}
	out := make([]ProfileDTO, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, ProfileDTO{ID: p.ID, Name: p.Name, ProxyDisplay: MaskProxy(p.ProxyURL), ProxyFingerprint: Fingerprint(p.ProxyURL), BoundNodeCount: bound[p.ID], UpdatedAt: p.UpdatedAt})
	}
	return out, nil
}

func (r *Registry) saveProfile(ctx context.Context, id string, req *http.Request) (any, error) {
	var in profileInput
	if err := decode(req, &in); err != nil {
		return nil, err
	}
	profiles, err := r.store.ListGrokEgressProxyProfiles(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	profile := &store.GrokEgressProxyProfile{ID: NewID(), CreatedAt: now}
	if id != "" {
		profile = nil
		for _, existing := range profiles {
			if existing.ID == id {
				copied := *existing
				profile = &copied
			}
		}
		if profile == nil {
			return nil, errNotFound
		}
	}
	profile.Name = strings.TrimSpace(in.Name)
	if proxy := strings.TrimSpace(in.ProxyURL); proxy != "" {
		profile.ProxyURL = proxy
	}
	switch {
	case profile.Name == "" || len(profile.Name) > 80:
		return nil, badRequest("name is required (at most 80 characters)")
	case profile.ProxyURL == "":
		return nil, badRequest("proxy URL is required")
	}
	if err := ValidateProxyURL(profile.ProxyURL); err != nil {
		return nil, badRequest("proxy: %s", err.Error())
	}
	profile.UpdatedAt = now
	if err := r.store.SaveGrokEgressProxyProfile(ctx, profile); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return ProfileDTO{ID: profile.ID, Name: profile.Name, ProxyDisplay: MaskProxy(profile.ProxyURL), ProxyFingerprint: Fingerprint(profile.ProxyURL), UpdatedAt: profile.UpdatedAt}, nil
}

func (r *Registry) deleteProfile(ctx context.Context, id string) (any, error) {
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if node.ProxyProfileID == id {
			return nil, &apiError{status: http.StatusConflict, message: "profile is still used by nodes"}
		}
	}
	if err := r.store.DeleteGrokEgressProxyProfile(ctx, id); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]bool{"deleted": true}, nil
}

// ── subscription sources ──────────────────────────────────────────────────

// SourceDTO is a subscription as the admin page sees it.
type SourceDTO struct {
	ID                     string     `json:"id"`
	Name                   string     `json:"name"`
	Scope                  string     `json:"scope"`
	Enabled                bool       `json:"enabled"`
	URLDisplay             string     `json:"url_display"`
	SubscriptionProxy      string     `json:"subscription_proxy,omitempty"`
	RefreshIntervalSeconds int        `json:"refresh_interval_seconds"`
	DefaultAccountCapacity int        `json:"default_account_capacity"`
	NodeCount              int        `json:"node_count"`
	LastSyncAt             *time.Time `json:"last_sync_at,omitempty"`
	LastSyncError          string     `json:"last_sync_error,omitempty"`
}

type sourceInput struct {
	Name                   string `json:"name"`
	Scope                  string `json:"scope"`
	Enabled                *bool  `json:"enabled"`
	URL                    string `json:"url"`
	SubscriptionProxyURL   string `json:"subscription_proxy_url"`
	ClearSubscriptionProxy bool   `json:"clear_subscription_proxy"`
	RefreshIntervalSeconds int    `json:"refresh_interval_seconds"`
	DefaultAccountCapacity int    `json:"default_account_capacity"`
}

func maskURL(raw string) string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(raw), "://")
	if !ok {
		return ""
	}
	host, _, _ := strings.Cut(rest, "/")
	if _, after, found := strings.Cut(host, "@"); found {
		host = after
	}
	return scheme + "://" + host + "/…"
}

func (r *Registry) serveSources(ctx context.Context, req *http.Request, id, action string) (any, error) {
	switch {
	case id == "" && req.Method == http.MethodGet:
		return r.listSources(ctx)
	case id == "" && req.Method == http.MethodPost:
		return r.saveSource(ctx, "", req)
	case id != "" && action == "" && req.Method == http.MethodPut:
		return r.saveSource(ctx, id, req)
	case id != "" && action == "" && req.Method == http.MethodDelete:
		return r.deleteSource(ctx, id)
	case id != "" && action == "sync" && req.Method == http.MethodPost:
		return r.SyncSource(ctx, id)
	}
	return nil, errNotFound
}

func (r *Registry) listSources(ctx context.Context) ([]SourceDTO, error) {
	sources, err := r.store.ListGrokEgressSources(ctx)
	if err != nil {
		return nil, err
	}
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, node := range nodes {
		counts[node.SourceID]++
	}
	out := make([]SourceDTO, 0, len(sources))
	for _, s := range sources {
		dto := SourceDTO{ID: s.ID, Name: s.Name, Scope: s.Scope, Enabled: s.Enabled, URLDisplay: maskURL(s.URL), SubscriptionProxy: MaskProxy(s.SubscriptionProxyURL),
			RefreshIntervalSeconds: s.RefreshIntervalSeconds, DefaultAccountCapacity: s.DefaultAccountCapacity, NodeCount: counts[s.ID], LastSyncError: s.LastSyncError}
		if !s.LastSyncAt.IsZero() {
			at := s.LastSyncAt
			dto.LastSyncAt = &at
		}
		out = append(out, dto)
	}
	return out, nil
}

func (r *Registry) findSource(ctx context.Context, id string) (*store.GrokEgressSource, error) {
	sources, err := r.store.ListGrokEgressSources(ctx)
	if err != nil {
		return nil, err
	}
	for _, source := range sources {
		if source.ID == id {
			return source, nil
		}
	}
	return nil, errNotFound
}

func (r *Registry) saveSource(ctx context.Context, id string, req *http.Request) (any, error) {
	var in sourceInput
	if err := decode(req, &in); err != nil {
		return nil, err
	}
	now := time.Now()
	source := &store.GrokEgressSource{ID: NewID(), Enabled: true, RefreshIntervalSeconds: 900, CreatedAt: now}
	if id != "" {
		existing, err := r.findSource(ctx, id)
		if err != nil {
			return nil, err
		}
		source = existing
	}
	source.Name = strings.TrimSpace(in.Name)
	source.Scope = strings.TrimSpace(in.Scope)
	if in.Enabled != nil {
		source.Enabled = *in.Enabled
	}
	if raw := strings.TrimSpace(in.URL); raw != "" {
		source.URL = raw
	}
	switch {
	case in.ClearSubscriptionProxy:
		source.SubscriptionProxyURL = ""
	case strings.TrimSpace(in.SubscriptionProxyURL) != "":
		source.SubscriptionProxyURL = strings.TrimSpace(in.SubscriptionProxyURL)
	}
	if in.RefreshIntervalSeconds != 0 {
		source.RefreshIntervalSeconds = in.RefreshIntervalSeconds
	}
	source.DefaultAccountCapacity = in.DefaultAccountCapacity
	switch {
	case source.Name == "" || len(source.Name) > 80:
		return nil, badRequest("name is required (at most 80 characters)")
	case !store.ValidGrokEgressScope(source.Scope):
		return nil, badRequest("unknown scope %q", source.Scope)
	case !strings.HasPrefix(source.URL, "https://") && !strings.HasPrefix(source.URL, "http://"):
		return nil, badRequest("subscription URL must be http(s)")
	case source.RefreshIntervalSeconds < 60 || source.RefreshIntervalSeconds > 86400:
		return nil, badRequest("refresh interval must be between 60 and 86400 seconds")
	case source.DefaultAccountCapacity < 0 || source.DefaultAccountCapacity > 100000:
		return nil, badRequest("default account capacity must be between 0 and 100000")
	}
	if source.SubscriptionProxyURL != "" {
		if _, err := ParseProxyURL(source.SubscriptionProxyURL); err != nil || IsTunnelURL(source.SubscriptionProxyURL) {
			return nil, badRequest("subscription proxy must be an http or socks5 address")
		}
	}
	source.UpdatedAt = now
	if err := r.store.SaveGrokEgressSource(ctx, source); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]string{"id": source.ID}, nil
}

func (r *Registry) deleteSource(ctx context.Context, id string) (any, error) {
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	var owned []string
	for _, node := range nodes {
		if node.SourceID == id {
			owned = append(owned, node.ID)
		}
	}
	if _, err := r.store.DeleteGrokEgressNodes(ctx, owned...); err != nil {
		return nil, err
	}
	if err := r.store.DeleteGrokEgressSource(ctx, id); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]int{"deleted_nodes": len(owned)}, nil
}

// SyncSource downloads a subscription and reconciles its nodes: exits keep
// their node (matched by address), new exits become nodes, and exits the
// subscription no longer lists are removed.
func (r *Registry) SyncSource(ctx context.Context, id string) (any, error) {
	source, err := r.findSource(ctx, id)
	if err != nil {
		return nil, err
	}
	entries, fetchErr := FetchSubscription(ctx, source.URL, source.SubscriptionProxyURL)
	source.LastSyncAt = time.Now()
	source.LastSyncError = ""
	if fetchErr != nil {
		source.LastSyncError = fetchErr.Error()
		_ = r.store.SaveGrokEgressSource(ctx, source)
		return nil, badRequest("sync failed: %s", fetchErr.Error())
	}
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	existing := map[string]*store.GrokEgressNode{}
	for _, node := range nodes {
		if node.SourceID == source.ID {
			existing[Fingerprint(node.ProxyURL)] = node
		}
	}
	now := time.Now()
	var upserts []*store.GrokEgressNode
	seen := map[string]bool{}
	created := 0
	for i, entry := range entries {
		key := Fingerprint(entry.ProxyURL)
		if seen[key] {
			continue
		}
		seen[key] = true
		name := entry.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", source.Name, i+1)
		}
		if len(name) > 80 {
			name = name[:80]
		}
		if node, ok := existing[key]; ok {
			if node.Name != name || node.Scope != source.Scope {
				node.Name, node.Scope, node.UpdatedAt = name, source.Scope, now
				upserts = append(upserts, node)
			}
			continue
		}
		created++
		upserts = append(upserts, &store.GrokEgressNode{ID: NewID(), Name: name, Scope: source.Scope, Enabled: source.Enabled, ProxyURL: entry.ProxyURL,
			AccountCapacity: source.DefaultAccountCapacity, SourceID: source.ID, CreatedAt: now, UpdatedAt: now})
	}
	var removed []string
	for key, node := range existing {
		if !seen[key] {
			removed = append(removed, node.ID)
		}
	}
	if err := r.store.SaveGrokEgressNodes(ctx, upserts); err != nil {
		return nil, err
	}
	if _, err := r.store.DeleteGrokEgressNodes(ctx, removed...); err != nil {
		return nil, err
	}
	if err := r.store.SaveGrokEgressSource(ctx, source); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]int{"entries": len(seen), "created": created, "removed": len(removed)}, nil
}
