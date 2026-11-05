package egressnodes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"orchids-api/internal/store"
	"strings"
	"sync"
	"time"
)

func (r *Registry) effectiveProxy(ctx context.Context, node *store.GrokEgressNode) (string, error) {
	if node.ProxyProfileID == "" {
		return node.ProxyURL, nil
	}
	profiles, err := r.store.ListGrokEgressProxyProfiles(ctx)
	if err != nil {
		return "", err
	}
	for _, profile := range profiles {
		if profile.ID == node.ProxyProfileID {
			return profile.ProxyURL, nil
		}
	}
	return "", errors.New("proxy profile missing")
}

func (r *Registry) probeProvider(ctx context.Context) string {
	ops, err := r.store.GetGrokEgressOperations(ctx)
	if err != nil {
		return "cloudflare"
	}
	return ops.ProbeProvider
}

func (r *Registry) testNode(ctx context.Context, id string) (*NodeDTO, error) {
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		if node.ID != id {
			continue
		}
		if err := r.probeAndSave(ctx, node, r.probeProvider(ctx)); err != nil {
			return nil, err
		}
		return r.nodeDTO(ctx, id)
	}
	return nil, errNotFound
}

func (r *Registry) probeAndSave(ctx context.Context, node *store.GrokEgressNode, provider string) error {
	proxy, err := r.effectiveProxy(ctx, node)
	var result *store.GrokEgressProbe
	if err != nil {
		result = &store.GrokEgressProbe{NodeID: node.ID, ProbedAt: time.Now(), Status: "unhealthy", Provider: provider, Error: err.Error()}
	} else {
		result = ProbeNode(ctx, node, proxy, provider)
	}
	return r.store.SaveGrokEgressProbe(ctx, result)
}

// ProbeAll tests nodes (all when ids is empty), eight at a time.
func (r *Registry) ProbeAll(ctx context.Context, ids []string) (map[string]int, error) {
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	provider := r.probeProvider(ctx)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var mu sync.Mutex
	result := map[string]int{"tested": 0}
	for _, node := range nodes {
		if len(wanted) > 0 && !wanted[node.ID] {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(node *store.GrokEgressNode) {
			defer wg.Done()
			defer func() { <-sem }()
			_ = r.probeAndSave(ctx, node, provider)
			mu.Lock()
			result["tested"]++
			mu.Unlock()
		}(node)
	}
	wg.Wait()
	probes, _ := r.store.ListGrokEgressProbes(ctx)
	for _, node := range nodes {
		if probe, ok := probes[node.ID]; ok && (len(wanted) == 0 || wanted[node.ID]) {
			result[probe.Status]++
		}
	}
	return result, nil
}

func (r *Registry) testAll(ctx context.Context, req *http.Request) (any, error) {
	var in struct {
		IDs []string `json:"ids"`
	}
	if req.ContentLength != 0 {
		if err := decode(req, &in); err != nil {
			return nil, err
		}
	}
	return r.ProbeAll(ctx, in.IDs)
}

func (r *Registry) refreshClearance(ctx context.Context, id string) (*NodeDTO, error) {
	hooks := currentClearanceHooks()
	if hooks.Refresh == nil {
		return nil, badRequest("clearance refresh is unavailable")
	}
	if err := hooks.Refresh(ctx, id); err != nil {
		return nil, badRequest("clearance refresh failed: %s", sanitizeProbeError(err))
	}
	return r.nodeDTO(ctx, id)
}

// cleanup previews (apply=false) or deletes nodes whose last probe failed.
func (r *Registry) cleanup(ctx context.Context, req *http.Request) (any, error) {
	var in struct {
		Apply bool `json:"apply"`
	}
	if req.ContentLength != 0 {
		if err := decode(req, &in); err != nil {
			return nil, err
		}
	}
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	probes, err := r.store.ListGrokEgressProbes(ctx)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Error string `json:"error,omitempty"`
	}
	var ids []string
	candidates := []candidate{}
	for _, node := range nodes {
		if probe, ok := probes[node.ID]; ok && probe.Status == "unhealthy" {
			ids = append(ids, node.ID)
			candidates = append(candidates, candidate{ID: node.ID, Name: node.Name, Error: probe.Error})
		}
	}
	if !in.Apply {
		return map[string]any{"candidates": candidates}, nil
	}
	return r.deleteNodes(ctx, ids)
}

type importRequest struct {
	Name            string `json:"name"`
	Scope           string `json:"scope"`
	AccountCapacity int    `json:"account_capacity"`
	Content         string `json:"content"`
}

func (r *Registry) importNodes(ctx context.Context, req *http.Request) (any, error) {
	var in importRequest
	if err := decode(req, &in); err != nil {
		return nil, err
	}
	if !store.ValidGrokEgressScope(in.Scope) {
		return nil, badRequest("unknown scope %q", in.Scope)
	}
	if in.AccountCapacity < 0 || in.AccountCapacity > 100000 {
		return nil, badRequest("account capacity must be between 0 and 100000")
	}
	entries := ParseProxyList(in.Content)
	if len(entries) == 0 {
		return nil, badRequest("no supported proxy addresses or share links found")
	}
	prefix := strings.TrimSpace(in.Name)
	if prefix == "" {
		prefix = "import"
	}
	now := time.Now()
	nodes := make([]*store.GrokEgressNode, 0, len(entries))
	for i, entry := range entries {
		name := entry.Name
		if name == "" || len(entries) > 1 && prefix != "import" {
			name = fmt.Sprintf("%s-%d", prefix, i+1)
		}
		if len(name) > 80 {
			name = name[:80]
		}
		nodes = append(nodes, &store.GrokEgressNode{ID: NewID(), Name: name, Scope: in.Scope, Enabled: true, ProxyURL: entry.ProxyURL,
			AccountCapacity: in.AccountCapacity, CreatedAt: now, UpdatedAt: now})
	}
	if err := r.store.SaveGrokEgressNodes(ctx, nodes); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]int{"created": len(nodes)}, nil
}
