// Package egress routes Grok Build (CLI) requests through the shared egress
// node pool (internal/grok/egresspool). It owns only what is Build-specific:
// a plain proxy-aware HTTP client per lease and the Build probe target.
//
// It is disabled by default (GrokEgressEnabled=false) so existing behavior is
// unchanged until configured. When enabled it is fail-closed: a request either
// gets a valid lease or an error — it never silently falls back to a direct
// client.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/egresspool"
	"orchids-api/internal/httpclient"
)

// FeedbackOutcome categorizes a lease result for node health scoring.
type FeedbackOutcome = egresspool.Outcome

const (
	OutcomeSuccess        = egresspool.OutcomeSuccess
	OutcomeTransportError = egresspool.OutcomeTransportError
	OutcomeServerError    = egresspool.OutcomeServerError
	OutcomeRateLimited    = egresspool.OutcomeRateLimited
	OutcomeAccountBlock   = egresspool.OutcomeAccountBlock
	OutcomeForbidden      = egresspool.OutcomeForbidden
)

// buildProfile holds the Build plane's pool settings. The health file name and
// metrics namespace predate the shared pool and are kept so persisted health
// and dashboards carry over.
var buildProfile = egresspool.Profile{
	Namespace:  "grok",
	HealthFile: ".egress-health-build.json",
	Probe:      probeBuildNode,
}

// Manager routes Build requests through the Build node pool.
type Manager struct {
	pool *egresspool.Pool
}

var errNoClient = errors.New("egress client not initialized")
var errLeaseReleased = errors.New("egress lease already released")

// NewManager builds a Build egress manager from configuration.
func NewManager(cfg *config.Config) *Manager {
	return &Manager{pool: egresspool.New(cfg, buildProfile)}
}

var (
	sharedMu      sync.Mutex
	sharedManager *Manager
)

// Shared returns the process Build egress manager, updated to cfg. Health,
// cooldowns and sticky routing survive configuration saves.
func Shared(cfg *config.Config) *Manager {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedManager == nil {
		sharedManager = NewManager(cfg)
		return sharedManager
	}
	sharedManager.pool.SetConfig(cfg)
	return sharedManager
}

// Routes reports whether requests for scope route through the manager.
func (m *Manager) Routes(scope string) bool {
	if m == nil {
		return false
	}
	return m.pool.Routes(scope)
}

// FeedbackOutcome updates a node's health from a classified outcome.
func (m *Manager) FeedbackOutcome(nodeID string, outcome FeedbackOutcome) {
	if m == nil {
		return
	}
	m.pool.Feedback(nodeID, outcome)
}

// Acquire selects a healthy Build proxy node and returns a sticky lease.
// affinity keeps the same account on the same exit. When no healthy node exists,
// Acquire fails closed.
func (m *Manager) Acquire(ctx context.Context, scope, affinity string) (*Lease, error) {
	if m == nil {
		return nil, errors.New("egress disabled")
	}
	node, direct, err := m.pool.Pick(ctx, scope, affinity)
	if err != nil {
		return nil, err
	}
	if direct {
		return &Lease{Direct: true}, nil
	}
	proxy, err := node.ProxyFor(affinity)
	if err != nil {
		return nil, fmt.Errorf("egress node %q: %w", node.Label, err)
	}
	proxyFunc, proxyKey := egresspool.ProxyFunc(proxy)
	// Isolate connection pools by node, proxy URL, and affinity binding. The
	// proxy component is hashed so credentials never appear in cache keys or
	// diagnostics, while a same-name node whose URL changes gets a fresh pool.
	fingerprint := egresspool.ShortHash(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(node.Name)),
		strings.TrimSpace(node.URL),
		strings.ToLower(strings.TrimSpace(affinity)),
	}, "\x00"))
	poolKey := "egress:" + node.Name + "|proxy=" + egresspool.ShortHash(proxyKey) + "|" + fingerprint
	client := httpclient.GetSharedHTTPClient(poolKey, m.pool.Config().UpstreamRequestTimeout(), proxyFunc)
	return &Lease{NodeID: node.Name, ProxyURL: node.URL, client: client}, nil
}

// probeBuildNode checks a node against the Build upstream.
func probeBuildNode(ctx context.Context, _ *config.Config, node egresspool.Node) error {
	proxy, err := node.ProxyFor("probe")
	if err != nil {
		return err
	}
	proxyFunc, _ := egresspool.ProxyFunc(proxy)
	client := httpclient.GetSharedHTTPClient("egress-probe:"+node.Name+"|proxy="+egresspool.ShortHash(node.URL), egresspool.ProbeTimeout, proxyFunc)
	return egresspool.ProbeThrough(ctx, client, "https://cli-chat-proxy.grok.com/", "orchids-2api-build-egress-probe")
}

// Lease is a pinned egress path for one Build request. A Direct lease means
// the scope fell back to the general proxy: the caller uses its own client.
type Lease struct {
	NodeID   string
	ProxyURL string
	Direct   bool
	client   *http.Client
	released atomic.Bool
}

// Do issues the request through the lease's proxy-aware client.
func (l *Lease) Do(req *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errNoClient
	}
	if l.released.Load() {
		return nil, errLeaseReleased
	}
	return l.client.Do(req)
}

// Release marks the lease closed. The HTTP client itself is shared
// process-wide, so no exclusive connection pool is owned by a lease.
func (l *Lease) Release() {
	if l != nil {
		l.released.Store(true)
	}
}
