package egressnodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/store"
)

// The admin surface is mounted under /api/grok/admin/egress/ behind the admin
// session. Proxy URLs, subscription URLs and Cloudflare cookies are
// write-only: responses carry a masked address and a fingerprint, never the
// credential itself.

// NodeDTO is a node as the admin page sees it.
type NodeDTO struct {
	ID                   string         `json:"id"`
	Name                 string         `json:"name"`
	Scope                string         `json:"scope"`
	Enabled              bool           `json:"enabled"`
	ProxyConfigured      bool           `json:"proxy_configured"`
	ProxyDisplay         string         `json:"proxy_display"`
	ProxyFingerprint     string         `json:"proxy_fingerprint,omitempty"`
	ProxyProtocol        string         `json:"proxy_protocol,omitempty"`
	ProxyPool            bool           `json:"proxy_pool"`
	ProxyProfileID       string         `json:"proxy_profile_id,omitempty"`
	ProxyProfileName     string         `json:"proxy_profile_name,omitempty"`
	SourceID             string         `json:"source_id,omitempty"`
	SourceName           string         `json:"source_name,omitempty"`
	AccountCapacity      int            `json:"account_capacity"`
	AssignedAccountCount int            `json:"assigned_account_count"`
	UserAgent            string         `json:"user_agent,omitempty"`
	CookieConfigured     bool           `json:"cookie_configured"`
	Routable             bool           `json:"routable"`
	InvalidReason        string         `json:"invalid_reason,omitempty"`
	Health               float64        `json:"health"`
	FailureCount         int            `json:"failure_count"`
	CooldownUntil        *time.Time     `json:"cooldown_until,omitempty"`
	LastError            string         `json:"last_error,omitempty"`
	Probe                *ProbeDTO      `json:"probe"`
	Clearance            *ClearanceInfo `json:"clearance,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}

// ProbeDTO is the last connectivity test.
type ProbeDTO struct {
	Status    string    `json:"status"`
	ProbedAt  time.Time `json:"probed_at"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	ExitIP    string    `json:"exit_ip,omitempty"`
	IPv4      string    `json:"ipv4,omitempty"`
	IPv6      string    `json:"ipv6,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// NodeInput is a create or update request. Absent pointer fields keep their
// value on update; an empty proxy_url or cloudflare_cookies keeps the stored
// one unless clear_proxy_url / clear_cookies is set.
type NodeInput struct {
	Name              *string `json:"name"`
	Scope             *string `json:"scope"`
	Enabled           *bool   `json:"enabled"`
	ProxyPool         *bool   `json:"proxy_pool"`
	ProxyURL          string  `json:"proxy_url"`
	ProxyProfileID    *string `json:"proxy_profile_id"`
	AccountCapacity   *int    `json:"account_capacity"`
	UserAgent         *string `json:"user_agent"`
	CloudflareCookies string  `json:"cloudflare_cookies"`
	ClearProxyURL     bool    `json:"clear_proxy_url"`
	ClearCookies      bool    `json:"clear_cookies"`
}

// DefaultUserAgents are the browser UAs each scope sends when a node sets none.
var DefaultUserAgents = map[string]string{
	store.GrokEgressScopeBuild:    "",
	store.GrokEgressScopeWeb:      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
	store.GrokEgressScopeWebAsset: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36",
}

type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(format string, args ...any) error {
	return &apiError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...)}
}

var errNotFound = &apiError{status: http.StatusNotFound, message: "not found"}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeErr(w http.ResponseWriter, err error) {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		httpserver.WriteError(w, apiErr.status, apiErr.message)
		return
	}
	if errors.Is(err, store.ErrGrokEgressNotFound) {
		httpserver.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	httpserver.WriteError(w, http.StatusInternalServerError, "egress operation failed")
}

func decode(r *http.Request, into any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(into); err != nil {
		return badRequest("invalid JSON body")
	}
	return nil
}

// ServeHTTP dispatches /api/grok/admin/egress/{resource}[/{id}[/{action}]].
func (r *Registry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if r == nil || r.store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "egress registry unavailable")
		return
	}
	rest := strings.Trim(strings.TrimPrefix(req.URL.Path, "/api/grok/admin/egress"), "/")
	parts := strings.Split(rest, "/")
	resource, id, action := parts[0], "", ""
	if len(parts) > 1 {
		id = parts[1]
	}
	if len(parts) > 2 {
		action = parts[2]
	}
	ctx := req.Context()
	var (
		result any
		err    error
		status = http.StatusOK
	)
	switch {
	case resource == "nodes" && id == "" && req.Method == http.MethodGet:
		result, err = r.listNodes(ctx, req)
	case resource == "nodes" && id == "" && req.Method == http.MethodPost:
		var in NodeInput
		if err = decode(req, &in); err == nil {
			result, err = r.saveNode(ctx, "", in)
			status = http.StatusCreated
		}
	case resource == "nodes" && id == "batch" && req.Method == http.MethodPost:
		result, err = r.batchNodes(ctx, req)
	case resource == "nodes" && id == "test-all" && req.Method == http.MethodPost:
		result, err = r.testAll(ctx, req)
	case resource == "nodes" && id == "cleanup" && req.Method == http.MethodPost:
		result, err = r.cleanup(ctx, req)
	case resource == "nodes" && action == "" && req.Method == http.MethodPut:
		var in NodeInput
		if err = decode(req, &in); err == nil {
			result, err = r.saveNode(ctx, id, in)
		}
	case resource == "nodes" && action == "" && req.Method == http.MethodDelete:
		result, err = r.deleteNodes(ctx, []string{id})
	case resource == "nodes" && action == "test" && req.Method == http.MethodPost:
		result, err = r.testNode(ctx, id)
	case resource == "nodes" && action == "refresh-clearance" && req.Method == http.MethodPost:
		result, err = r.refreshClearance(ctx, id)
	case resource == "imports" && req.Method == http.MethodPost:
		result, err = r.importNodes(ctx, req)
		status = http.StatusCreated
	case resource == "profiles":
		result, err = r.serveProfiles(ctx, req, id)
	case resource == "sources":
		result, err = r.serveSources(ctx, req, id, action)
	case resource == "operations" && id == "" && req.Method == http.MethodGet:
		result, err = r.store.GetGrokEgressOperations(ctx)
	case resource == "operations" && id == "" && req.Method == http.MethodPut:
		result, err = r.saveOperations(ctx, req)
	case resource == "operations" && id == "rebalance" && req.Method == http.MethodPost:
		result, err = r.Rebalance(ctx, Current().Operations.AutoBalanceEnabled || req.URL.Query().Get("balance") == "1")
	default:
		httpserver.WriteError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, status, result)
}

// ── nodes ─────────────────────────────────────────────────────────────────

type nodeListResponse struct {
	Items             []NodeDTO         `json:"items"`
	Total             int               `json:"total"`
	Page              int               `json:"page"`
	PageSize          int               `json:"page_size"`
	DefaultUserAgents map[string]string `json:"default_user_agents"`
	ClearanceMode     string            `json:"clearance_mode"`
	Summary           map[string]int    `json:"summary"`
}

func (r *Registry) nodeDTOs(ctx context.Context) ([]NodeDTO, error) {
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	profiles, _ := r.store.ListGrokEgressProxyProfiles(ctx)
	sources, _ := r.store.ListGrokEgressSources(ctx)
	probes, _ := r.store.ListGrokEgressProbes(ctx)
	profileByID := map[string]*store.GrokEgressProxyProfile{}
	for _, p := range profiles {
		profileByID[p.ID] = p
	}
	sourceName := map[string]string{}
	for _, s := range sources {
		sourceName[s.ID] = s.Name
	}
	snap := Current()
	bound := snap.BoundCount()
	hooks := currentClearanceHooks()
	out := make([]NodeDTO, 0, len(nodes))
	for _, node := range nodes {
		proxy := node.ProxyURL
		dto := NodeDTO{ID: node.ID, Name: node.Name, Scope: node.Scope, Enabled: node.Enabled, ProxyPool: node.ProxyPool, ProxyProfileID: node.ProxyProfileID,
			SourceID: node.SourceID, SourceName: sourceName[node.SourceID], AccountCapacity: node.AccountCapacity, AssignedAccountCount: bound[node.ID],
			UserAgent: node.UserAgent, CookieConfigured: node.CloudflareCookies != "", CreatedAt: node.CreatedAt, UpdatedAt: node.UpdatedAt}
		if profile, ok := profileByID[node.ProxyProfileID]; ok {
			proxy = profile.ProxyURL
			dto.ProxyProfileName = profile.Name
		}
		dto.ProxyConfigured = strings.TrimSpace(proxy) != ""
		dto.ProxyDisplay = MaskProxy(proxy)
		dto.ProxyFingerprint = Fingerprint(proxy)
		if scheme, _, ok := strings.Cut(proxy, "://"); ok {
			dto.ProxyProtocol = strings.ToLower(scheme)
		}
		_, dto.Routable = snap.Node(node.ID)
		dto.InvalidReason = snap.Invalid[node.ID]
		if health, ok := HealthOf(node.ID); ok {
			dto.Health, dto.FailureCount, dto.LastError = health.Score, health.Failures, health.LastError
			if health.CooldownUntil.After(time.Now()) {
				until := health.CooldownUntil
				dto.CooldownUntil = &until
			}
		} else {
			dto.Health = -1
		}
		if probe, ok := probes[node.ID]; ok {
			dto.Probe = &ProbeDTO{Status: probe.Status, ProbedAt: probe.ProbedAt, LatencyMS: probe.LatencyMS, ExitIP: probe.ExitIP, IPv4: probe.IPv4, IPv6: probe.IPv6, Provider: probe.Provider, Error: probe.Error}
		}
		if UsesClearance(node.Scope) && hooks.Inspect != nil {
			info := hooks.Inspect(node.ID)
			dto.Clearance = &info
		}
		out = append(out, dto)
	}
	return out, nil
}

func (r *Registry) listNodes(ctx context.Context, req *http.Request) (*nodeListResponse, error) {
	all, err := r.nodeDTOs(ctx)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))
	summary := map[string]int{"total": len(all)}
	var filtered []NodeDTO
	for _, node := range all {
		if node.Enabled {
			summary["enabled"]++
		}
		if node.Probe != nil && node.Probe.Status == "healthy" {
			summary["healthy"]++
		}
		if node.Probe != nil && node.Probe.Status == "unhealthy" {
			summary["unhealthy"]++
		}
		if search != "" && !strings.Contains(strings.ToLower(node.Name+" "+node.ProxyDisplay+" "+node.ProxyFingerprint+" "+node.SourceName), search) {
			continue
		}
		if scope := q.Get("scope"); scope != "" && node.Scope != scope {
			continue
		}
		if enabled := q.Get("enabled"); enabled != "" && strconv.FormatBool(node.Enabled) != enabled {
			continue
		}
		if probe := q.Get("probe"); probe != "" {
			status := "unknown"
			if node.Probe != nil {
				status = node.Probe.Status
			}
			if status != probe {
				continue
			}
		}
		if assignment := q.Get("assignment"); (assignment == "bound" && node.AssignedAccountCount == 0) || (assignment == "unbound" && node.AssignedAccountCount > 0) {
			continue
		}
		filtered = append(filtered, node)
	}
	sortBy, desc := q.Get("sort_by"), q.Get("sort_order") != "asc"
	if sortBy == "" {
		sortBy, desc = "health", true
	}
	compare := func(a, b NodeDTO) int {
		switch sortBy {
		case "name":
			return strings.Compare(a.Name, b.Name)
		case "accounts":
			return a.AssignedAccountCount - b.AssignedAccountCount
		case "latency":
			return int(min(max(latencyOf(a)-latencyOf(b), -1), 1))
		case "created":
			return a.CreatedAt.Compare(b.CreatedAt)
		default:
			switch {
			case a.Health < b.Health:
				return -1
			case a.Health > b.Health:
				return 1
			}
			return 0
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if desc {
			return compare(filtered[i], filtered[j]) > 0
		}
		return compare(filtered[i], filtered[j]) < 0
	})
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	page = max(page, 1)
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}
	start := min((page-1)*pageSize, len(filtered))
	end := min(start+pageSize, len(filtered))
	cfg := r.Config()
	mode := "manual"
	if cfg != nil {
		mode = cfg.GrokClearanceModeOrDefault()
	}
	items := filtered[start:end]
	if items == nil {
		items = []NodeDTO{}
	}
	return &nodeListResponse{Items: items, Total: len(filtered), Page: page, PageSize: pageSize, DefaultUserAgents: DefaultUserAgents, ClearanceMode: mode, Summary: summary}, nil
}

func latencyOf(n NodeDTO) int64 {
	if n.Probe == nil || n.Probe.Status != "healthy" {
		return 1 << 40
	}
	return n.Probe.LatencyMS
}

// normalizeCookies accepts a bare cf_clearance value or a cookie string.
func normalizeCookies(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "=") {
		return "cf_clearance=" + raw
	}
	var parts []string
	for _, item := range strings.Split(raw, ";") {
		item = strings.TrimSpace(item)
		if item != "" {
			parts = append(parts, item)
		}
	}
	return strings.Join(parts, "; ")
}

func (r *Registry) saveNode(ctx context.Context, id string, in NodeInput) (*NodeDTO, error) {
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	var node *store.GrokEgressNode
	for _, existing := range nodes {
		if existing.ID == id {
			copied := *existing
			node = &copied
		}
	}
	now := time.Now()
	if id == "" {
		node = &store.GrokEgressNode{ID: NewID(), Scope: store.GrokEgressScopeWeb, Enabled: true, CreatedAt: now}
	} else if node == nil {
		return nil, errNotFound
	}
	if in.Name != nil {
		node.Name = strings.TrimSpace(*in.Name)
	}
	if in.Scope != nil {
		node.Scope = strings.TrimSpace(*in.Scope)
	}
	if in.Enabled != nil {
		node.Enabled = *in.Enabled
	}
	if in.ProxyPool != nil {
		node.ProxyPool = *in.ProxyPool
	}
	if in.AccountCapacity != nil {
		node.AccountCapacity = *in.AccountCapacity
	}
	if in.UserAgent != nil {
		node.UserAgent = strings.TrimSpace(*in.UserAgent)
	}
	if in.ProxyProfileID != nil {
		node.ProxyProfileID = strings.TrimSpace(*in.ProxyProfileID)
	}
	switch {
	case in.ClearProxyURL:
		node.ProxyURL = ""
	case strings.TrimSpace(in.ProxyURL) != "":
		node.ProxyURL = strings.TrimSpace(in.ProxyURL)
	}
	switch {
	case in.ClearCookies:
		node.CloudflareCookies = ""
	case strings.TrimSpace(in.CloudflareCookies) != "":
		node.CloudflareCookies = normalizeCookies(in.CloudflareCookies)
	}
	if node.ProxyProfileID != "" {
		node.ProxyURL = ""
	}
	if err := r.validateNode(ctx, node); err != nil {
		return nil, err
	}
	node.UpdatedAt = now
	if err := r.store.SaveGrokEgressNode(ctx, node); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return r.nodeDTO(ctx, node.ID)
}

func (r *Registry) validateNode(ctx context.Context, node *store.GrokEgressNode) error {
	switch {
	case node.Name == "":
		return badRequest("name is required")
	case len(node.Name) > 80:
		return badRequest("name is longer than 80 characters")
	case !store.ValidGrokEgressScope(node.Scope):
		return badRequest("unknown scope %q", node.Scope)
	case node.AccountCapacity < 0 || node.AccountCapacity > 100000:
		return badRequest("account capacity must be between 0 and 100000")
	case len(node.UserAgent) > 512:
		return badRequest("user agent is longer than 512 characters")
	case len(node.CloudflareCookies) > 8192:
		return badRequest("cookies are longer than 8 KiB")
	case strings.Count(node.ProxyURL, "{account}") > 1:
		return badRequest("{account} may appear at most once")
	}
	if node.ProxyProfileID != "" {
		profiles, err := r.store.ListGrokEgressProxyProfiles(ctx)
		if err != nil {
			return err
		}
		found := false
		for _, profile := range profiles {
			found = found || profile.ID == node.ProxyProfileID
		}
		if !found {
			return badRequest("proxy profile not found")
		}
	}
	if err := ValidateProxyURL(node.ProxyURL); err != nil {
		return badRequest("proxy: %s", err.Error())
	}
	return nil
}

func (r *Registry) nodeDTO(ctx context.Context, id string) (*NodeDTO, error) {
	all, err := r.nodeDTOs(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}
	return nil, errNotFound
}

func (r *Registry) deleteNodes(ctx context.Context, ids []string) (map[string]int64, error) {
	deleted, err := r.store.DeleteGrokEgressNodes(ctx, ids...)
	if err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]int64{"deleted": deleted}, nil
}

type batchRequest struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

func (r *Registry) batchNodes(ctx context.Context, req *http.Request) (any, error) {
	var in batchRequest
	if err := decode(req, &in); err != nil {
		return nil, err
	}
	if len(in.IDs) == 0 {
		return nil, badRequest("ids are required")
	}
	if in.Action == "delete" {
		return r.deleteNodes(ctx, in.IDs)
	}
	if in.Action != "enable" && in.Action != "disable" {
		return nil, badRequest("action must be enable, disable or delete")
	}
	nodes, err := r.store.ListGrokEgressNodes(ctx)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, id := range in.IDs {
		wanted[id] = true
	}
	var changed []*store.GrokEgressNode
	for _, node := range nodes {
		if wanted[node.ID] && node.Enabled != (in.Action == "enable") {
			node.Enabled = in.Action == "enable"
			node.UpdatedAt = time.Now()
			changed = append(changed, node)
		}
	}
	if err := r.store.SaveGrokEgressNodes(ctx, changed); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return map[string]int{"updated": len(changed)}, nil
}

// ── operations ────────────────────────────────────────────────────────────

func (r *Registry) saveOperations(ctx context.Context, req *http.Request) (any, error) {
	var ops store.GrokEgressOperations
	if err := decode(req, &ops); err != nil {
		return nil, err
	}
	ops.Normalize()
	for scope, fallback := range ops.Fallbacks {
		if fallback.Mode != "fixed" {
			continue
		}
		node, ok := Current().Node(fallback.NodeID)
		if !ok {
			return nil, badRequest("fallback node for %s is not routable", scope)
		}
		if node.Pool || node.Direct() {
			return nil, badRequest("fallback node for %s must be a fixed proxy", scope)
		}
	}
	if err := r.store.SaveGrokEgressOperations(ctx, ops); err != nil {
		return nil, err
	}
	r.Changed(ctx)
	return r.store.GetGrokEgressOperations(ctx)
}
