package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Grok egress state: exit nodes, reusable proxy profiles, subscription sources,
// probe results, account bindings and the automation settings that drive them.
// Proxy URLs and Cloudflare cookies are credentials: they are encrypted at rest
// with the credential key and never leave the server through the admin API,
// which only ever returns a masked display form.

// Egress scopes. Web nodes can also serve Web asset downloads.
const (
	GrokEgressScopeBuild    = "grok_build"
	GrokEgressScopeWeb      = "grok_web"
	GrokEgressScopeWebAsset = "grok_web_asset"
)

// GrokEgressScopes lists every scope in display order.
var GrokEgressScopes = []string{GrokEgressScopeBuild, GrokEgressScopeWeb, GrokEgressScopeWebAsset}

// ValidGrokEgressScope reports whether scope names a known egress scope.
func ValidGrokEgressScope(scope string) bool {
	for _, known := range GrokEgressScopes {
		if scope == known {
			return true
		}
	}
	return false
}

// ErrGrokEgressNotFound is returned when a node, profile or source is missing.
var ErrGrokEgressNotFound = errors.New("grok egress record not found")

// GrokEgressNode is one exit. ProxyURL empty with no profile means direct.
type GrokEgressNode struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Scope             string    `json:"scope"`
	Enabled           bool      `json:"enabled"`
	ProxyURL          string    `json:"proxy_url,omitempty"`
	ProxyProfileID    string    `json:"proxy_profile_id,omitempty"`
	ProxyPool         bool      `json:"proxy_pool,omitempty"`
	AccountCapacity   int       `json:"account_capacity,omitempty"`
	UserAgent         string    `json:"user_agent,omitempty"`
	CloudflareCookies string    `json:"cloudflare_cookies,omitempty"`
	SourceID          string    `json:"source_id,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// GrokEgressProxyProfile is a named proxy address several nodes can share.
type GrokEgressProxyProfile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	ProxyURL  string    `json:"proxy_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// GrokEgressSource is a subscription whose share links become nodes.
type GrokEgressSource struct {
	ID                     string    `json:"id"`
	Name                   string    `json:"name"`
	Scope                  string    `json:"scope"`
	Enabled                bool      `json:"enabled"`
	URL                    string    `json:"url"`
	SubscriptionProxyURL   string    `json:"subscription_proxy_url,omitempty"`
	RefreshIntervalSeconds int       `json:"refresh_interval_seconds"`
	DefaultAccountCapacity int       `json:"default_account_capacity,omitempty"`
	LastSyncAt             time.Time `json:"last_sync_at,omitempty"`
	LastSyncError          string    `json:"last_sync_error,omitempty"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}

// GrokEgressProbe is the last connectivity test of a node.
type GrokEgressProbe struct {
	NodeID    string    `json:"node_id"`
	Status    string    `json:"status"` // unknown|healthy|unhealthy
	ProbedAt  time.Time `json:"probed_at"`
	LatencyMS int64     `json:"latency_ms,omitempty"`
	ExitIP    string    `json:"exit_ip,omitempty"`
	IPv4      string    `json:"ipv4,omitempty"`
	IPv6      string    `json:"ipv6,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Error     string    `json:"error,omitempty"`
}

// GrokEgressFallback says what a scope does when none of its nodes can serve.
type GrokEgressFallback struct {
	Mode   string `json:"mode"` // none|direct|fixed
	NodeID string `json:"node_id,omitempty"`
}

// GrokEgressOperations holds the automation settings.
type GrokEgressOperations struct {
	ProbeProvider             string                        `json:"probe_provider"`
	ProbeIntervalSeconds      int                           `json:"probe_interval_seconds"`
	AssignmentIntervalSeconds int                           `json:"assignment_interval_seconds"`
	AutoAssignEnabled         bool                          `json:"auto_assign_enabled"`
	AutoBalanceEnabled        bool                          `json:"auto_balance_enabled"`
	Fallbacks                 map[string]GrokEgressFallback `json:"fallbacks"`
}

// DefaultGrokEgressOperations returns the settings used before any are saved.
// A scope without nodes keeps using the general proxy (direct fallback), which
// is what a deployment without egress nodes has always done.
func DefaultGrokEgressOperations() GrokEgressOperations {
	ops := GrokEgressOperations{ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300, Fallbacks: map[string]GrokEgressFallback{}}
	for _, scope := range GrokEgressScopes {
		ops.Fallbacks[scope] = GrokEgressFallback{Mode: "direct"}
	}
	return ops
}

// Normalize fills missing values with defaults and clamps intervals.
func (o *GrokEgressOperations) Normalize() {
	defaults := DefaultGrokEgressOperations()
	if o.ProbeProvider != "ipinfo" {
		o.ProbeProvider = "cloudflare"
	}
	o.ProbeIntervalSeconds = clampSeconds(o.ProbeIntervalSeconds, defaults.ProbeIntervalSeconds)
	o.AssignmentIntervalSeconds = clampSeconds(o.AssignmentIntervalSeconds, defaults.AssignmentIntervalSeconds)
	if o.Fallbacks == nil {
		o.Fallbacks = map[string]GrokEgressFallback{}
	}
	for _, scope := range GrokEgressScopes {
		fallback, ok := o.Fallbacks[scope]
		switch {
		case !ok:
			fallback = defaults.Fallbacks[scope]
		case fallback.Mode == "fixed" && strings.TrimSpace(fallback.NodeID) != "":
		case fallback.Mode == "none":
			fallback.NodeID = ""
		default:
			fallback = GrokEgressFallback{Mode: "direct"}
		}
		o.Fallbacks[scope] = fallback
	}
	for scope := range o.Fallbacks {
		if !ValidGrokEgressScope(scope) {
			delete(o.Fallbacks, scope)
		}
	}
}

func clampSeconds(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return min(max(value, 60), 86400)
}

func (s *redisStore) egressKey(name string) string { return s.prefix + "grok:egress:" + name }

func (s *Store) egressRedis() (*redisStore, error) {
	if s == nil || s.webMedia == nil {
		return nil, errors.New("store unavailable")
	}
	return s.webMedia, nil
}

func (s *redisStore) seal(values ...*string) error {
	for _, value := range values {
		encrypted, err := s.credentials.encrypt(*value)
		if err != nil {
			return err
		}
		*value = encrypted
	}
	return nil
}

func (s *redisStore) open(values ...*string) error {
	for _, value := range values {
		plain, err := s.credentials.decrypt(*value)
		if err != nil {
			return err
		}
		*value = plain
	}
	return nil
}

// GrokEgressVersion is bumped on every egress change so other instances reload.
func (s *Store) GrokEgressVersion(ctx context.Context) (int64, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return 0, err
	}
	value, err := rs.client.Get(ctx, rs.egressKey("version")).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return value, err
}

func (s *redisStore) bumpEgress(ctx context.Context, pipe redis.Pipeliner) {
	pipe.Incr(ctx, s.egressKey("version"))
}

// hash-backed record helpers shared by nodes, profiles and sources.
func egressList[T any](ctx context.Context, rs *redisStore, bucket string, decode func(*T) error) ([]*T, error) {
	values, err := rs.client.HGetAll(ctx, rs.egressKey(bucket)).Result()
	if err != nil {
		return nil, err
	}
	out := make([]*T, 0, len(values))
	for id, raw := range values {
		var item T
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			return nil, fmt.Errorf("decode egress %s %s: %w", bucket, id, err)
		}
		if decode != nil {
			if err := decode(&item); err != nil {
				return nil, fmt.Errorf("decrypt egress %s %s: %w", bucket, id, err)
			}
		}
		out = append(out, &item)
	}
	return out, nil
}

func egressPut(ctx context.Context, rs *redisStore, bucket, id string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	pipe := rs.client.TxPipeline()
	pipe.HSet(ctx, rs.egressKey(bucket), id, raw)
	rs.bumpEgress(ctx, pipe)
	_, err = pipe.Exec(ctx)
	return err
}

func egressDelete(ctx context.Context, rs *redisStore, bucket string, ids ...string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	pipe := rs.client.TxPipeline()
	deleted := pipe.HDel(ctx, rs.egressKey(bucket), ids...)
	rs.bumpEgress(ctx, pipe)
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	return deleted.Val(), nil
}

// ListGrokEgressNodes returns every node, credentials decrypted, sorted by name.
func (s *Store) ListGrokEgressNodes(ctx context.Context) ([]*GrokEgressNode, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return nil, err
	}
	nodes, err := egressList(ctx, rs, "nodes", func(n *GrokEgressNode) error { return rs.open(&n.ProxyURL, &n.CloudflareCookies) })
	if err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool {
		if nodes[i].Name != nodes[j].Name {
			return nodes[i].Name < nodes[j].Name
		}
		return nodes[i].ID < nodes[j].ID
	})
	return nodes, nil
}

// SaveGrokEgressNode creates or replaces a node.
func (s *Store) SaveGrokEgressNode(ctx context.Context, node *GrokEgressNode) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	if node == nil || strings.TrimSpace(node.ID) == "" {
		return errors.New("egress node id is required")
	}
	stored := *node
	if err := rs.seal(&stored.ProxyURL, &stored.CloudflareCookies); err != nil {
		return err
	}
	return egressPut(ctx, rs, "nodes", stored.ID, &stored)
}

// SaveGrokEgressNodes replaces several nodes in one transaction.
func (s *Store) SaveGrokEgressNodes(ctx context.Context, nodes []*GrokEgressNode) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return nil
	}
	pipe := rs.client.TxPipeline()
	for _, node := range nodes {
		stored := *node
		if err := rs.seal(&stored.ProxyURL, &stored.CloudflareCookies); err != nil {
			return err
		}
		raw, err := json.Marshal(&stored)
		if err != nil {
			return err
		}
		pipe.HSet(ctx, rs.egressKey("nodes"), stored.ID, raw)
	}
	rs.bumpEgress(ctx, pipe)
	_, err = pipe.Exec(ctx)
	return err
}

// DeleteGrokEgressNodes removes nodes with their probes and account bindings.
func (s *Store) DeleteGrokEgressNodes(ctx context.Context, ids ...string) (int64, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return 0, err
	}
	if len(ids) > 0 {
		rs.client.HDel(ctx, rs.egressKey("probes"), ids...)
		if err := s.unbindGrokEgressNodes(ctx, ids...); err != nil {
			return 0, err
		}
	}
	return egressDelete(ctx, rs, "nodes", ids...)
}

// ListGrokEgressProxyProfiles returns every profile, decrypted, sorted by name.
func (s *Store) ListGrokEgressProxyProfiles(ctx context.Context) ([]*GrokEgressProxyProfile, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return nil, err
	}
	profiles, err := egressList(ctx, rs, "profiles", func(p *GrokEgressProxyProfile) error { return rs.open(&p.ProxyURL) })
	if err != nil {
		return nil, err
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// SaveGrokEgressProxyProfile creates or replaces a profile.
func (s *Store) SaveGrokEgressProxyProfile(ctx context.Context, profile *GrokEgressProxyProfile) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	stored := *profile
	if err := rs.seal(&stored.ProxyURL); err != nil {
		return err
	}
	return egressPut(ctx, rs, "profiles", stored.ID, &stored)
}

// DeleteGrokEgressProxyProfile removes a profile.
func (s *Store) DeleteGrokEgressProxyProfile(ctx context.Context, id string) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	deleted, err := egressDelete(ctx, rs, "profiles", id)
	if err == nil && deleted == 0 {
		return ErrGrokEgressNotFound
	}
	return err
}

// ListGrokEgressSources returns every subscription source, decrypted.
func (s *Store) ListGrokEgressSources(ctx context.Context) ([]*GrokEgressSource, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return nil, err
	}
	sources, err := egressList(ctx, rs, "sources", func(src *GrokEgressSource) error { return rs.open(&src.URL, &src.SubscriptionProxyURL) })
	if err != nil {
		return nil, err
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Name < sources[j].Name })
	return sources, nil
}

// SaveGrokEgressSource creates or replaces a source.
func (s *Store) SaveGrokEgressSource(ctx context.Context, source *GrokEgressSource) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	stored := *source
	if err := rs.seal(&stored.URL, &stored.SubscriptionProxyURL); err != nil {
		return err
	}
	return egressPut(ctx, rs, "sources", stored.ID, &stored)
}

// DeleteGrokEgressSource removes a source; its nodes are removed by the caller.
func (s *Store) DeleteGrokEgressSource(ctx context.Context, id string) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	deleted, err := egressDelete(ctx, rs, "sources", id)
	if err == nil && deleted == 0 {
		return ErrGrokEgressNotFound
	}
	return err
}

// ListGrokEgressProbes returns the last probe of every node by node ID.
func (s *Store) ListGrokEgressProbes(ctx context.Context) (map[string]*GrokEgressProbe, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return nil, err
	}
	probes, err := egressList[GrokEgressProbe](ctx, rs, "probes", nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*GrokEgressProbe, len(probes))
	for _, probe := range probes {
		out[probe.NodeID] = probe
	}
	return out, nil
}

// SaveGrokEgressProbe records a probe result. Probes do not bump the version:
// they change no routing input.
func (s *Store) SaveGrokEgressProbe(ctx context.Context, probe *GrokEgressProbe) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	return rs.client.HSet(ctx, rs.egressKey("probes"), probe.NodeID, raw).Err()
}

// GetGrokEgressOperations returns the automation settings, defaulted.
func (s *Store) GetGrokEgressOperations(ctx context.Context) (GrokEgressOperations, error) {
	ops := DefaultGrokEgressOperations()
	rs, err := s.egressRedis()
	if err != nil {
		return ops, err
	}
	raw, err := rs.client.Get(ctx, rs.egressKey("operations")).Bytes()
	if errors.Is(err, redis.Nil) {
		return ops, nil
	}
	if err != nil {
		return ops, err
	}
	var stored GrokEgressOperations
	if err := json.Unmarshal(raw, &stored); err != nil {
		return ops, err
	}
	stored.Normalize()
	return stored, nil
}

// SaveGrokEgressOperations stores the automation settings.
func (s *Store) SaveGrokEgressOperations(ctx context.Context, ops GrokEgressOperations) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	ops.Normalize()
	raw, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	pipe := rs.client.TxPipeline()
	pipe.Set(ctx, rs.egressKey("operations"), raw, 0)
	rs.bumpEgress(ctx, pipe)
	_, err = pipe.Exec(ctx)
	return err
}

// Account bindings pin an account to one node per scope. They live in one hash
// keyed "scope|account" so a node's bound accounts can be counted in one read.

func grokEgressBindingField(scope, account string) string { return scope + "|" + account }

// ListGrokEgressBindings returns scope|account -> node ID.
func (s *Store) ListGrokEgressBindings(ctx context.Context) (map[string]string, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return nil, err
	}
	return rs.client.HGetAll(ctx, rs.egressKey("bindings")).Result()
}

// SetGrokEgressBinding pins (or, with an empty node, unpins) an account.
func (s *Store) SetGrokEgressBinding(ctx context.Context, scope, account, nodeID string) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	field := grokEgressBindingField(scope, account)
	if nodeID == "" {
		return rs.client.HDel(ctx, rs.egressKey("bindings"), field).Err()
	}
	return rs.client.HSet(ctx, rs.egressKey("bindings"), field, nodeID).Err()
}

// ReplaceGrokEgressBindings swaps the whole binding table, used by rebalancing.
func (s *Store) ReplaceGrokEgressBindings(ctx context.Context, bindings map[string]string) error {
	rs, err := s.egressRedis()
	if err != nil {
		return err
	}
	pipe := rs.client.TxPipeline()
	pipe.Del(ctx, rs.egressKey("bindings"))
	if len(bindings) > 0 {
		values := make([]any, 0, len(bindings)*2)
		for field, node := range bindings {
			values = append(values, field, node)
		}
		pipe.HSet(ctx, rs.egressKey("bindings"), values...)
	}
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Store) unbindGrokEgressNodes(ctx context.Context, ids ...string) error {
	bindings, err := s.ListGrokEgressBindings(ctx)
	if err != nil {
		return err
	}
	removed := map[string]bool{}
	for _, id := range ids {
		removed[id] = true
	}
	var fields []string
	for field, node := range bindings {
		if removed[node] {
			fields = append(fields, field)
		}
	}
	if len(fields) == 0 {
		return nil
	}
	rs, _ := s.egressRedis()
	return rs.client.HDel(ctx, rs.egressKey("bindings"), fields...).Err()
}

// ClaimGrokEgressJob takes a short lease on a scheduled egress job so only one
// instance runs it per interval.
func (s *Store) ClaimGrokEgressJob(ctx context.Context, job string, ttl time.Duration) (bool, error) {
	rs, err := s.egressRedis()
	if err != nil {
		return false, err
	}
	return rs.client.SetNX(ctx, rs.egressKey("job:"+job), time.Now().UnixMilli(), ttl).Result()
}
