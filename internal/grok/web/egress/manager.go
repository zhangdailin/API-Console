// Package egress routes Grok Web requests through the shared egress node pool
// (internal/grok/egresspool). It owns what is Web-specific: Cloudflare
// clearance per node and fingerprint, User-Agent rotation, and a browser TLS
// client whose ClientHello matches the advertised UA.
//
// It is disabled by default (GrokEgressEnabled=false) so existing behavior is
// unchanged until configured. When enabled it is fail-closed: a request either
// gets a valid lease with clearance, or an error — it never silently falls
// back to a direct client.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/egressnodes"
	"orchids-api/internal/grok/egresspool"
	"orchids-api/internal/httpclient"
)

// FeedbackOutcome categorizes a lease result for node health scoring.
type FeedbackOutcome = egresspool.Outcome

const (
	OutcomeSuccess        = egresspool.OutcomeSuccess
	OutcomeTransportError = egresspool.OutcomeTransportError
	OutcomeServerError    = egresspool.OutcomeServerError
	OutcomeChallenge      = egresspool.OutcomeChallenge
	OutcomeRateLimited    = egresspool.OutcomeRateLimited
	OutcomeAccountBlock   = egresspool.OutcomeAccountBlock
	OutcomeForbidden      = egresspool.OutcomeForbidden
)

// Node is one egress exit of the shared pool.
type Node = egresspool.Node

// webProfile holds the Web plane's pool settings. The health file name and
// metrics namespace predate the shared pool and are kept so persisted health
// and dashboards carry over.
var webProfile = egresspool.Profile{
	Namespace:  "grok_web",
	HealthFile: ".egress-health.json",
	Probe:      probeWebNode,
}

// Manager routes Web requests through the Web node pool and binds a
// User-Agent and Cloudflare clearance to every lease.
type Manager struct {
	pool *egresspool.Pool

	mu         sync.Mutex // guards the clearance state below
	clearances map[string]clearanceState
	version    uint64 // clearance generation counter
	solver     clearanceSolver
}

type clearanceState struct {
	nodeID      string
	cookies     string
	userAgent   string
	refreshedAt time.Time
	invalid     bool
	version     uint64
}

var errNoClient = errors.New("egress client not initialized")
var errClearanceUnavailable = errors.New("egress clearance unavailable")

// NewManager builds a Web egress manager from configuration.
func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		pool:       egresspool.New(cfg, webProfile),
		clearances: make(map[string]clearanceState),
		solver:     flaresolverrSolver{},
	}
}

var (
	sharedMu      sync.Mutex
	sharedManager *Manager
)

// Shared returns the process Web egress manager, updated to cfg. Health,
// stickiness and Cloudflare clearance survive configuration saves, and the
// registry's clearance hooks are bound to it.
func Shared(cfg *config.Config) *Manager {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if sharedManager == nil {
		sharedManager = NewManager(cfg)
		manager := sharedManager
		egressnodes.SetClearanceHooks(egressnodes.ClearanceHooks{Inspect: manager.InspectClearance, Refresh: manager.RefreshClearance})
		return sharedManager
	}
	sharedManager.pool.SetConfig(cfg)
	return sharedManager
}

func (m *Manager) config() *config.Config {
	if m == nil || m.pool == nil {
		return nil
	}
	return m.pool.Config()
}

// Routes reports whether requests for scope route through the manager: the
// scope has nodes, or its fallback is not the general proxy.
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

// Acquire selects a healthy node for a scope, binding a UA + clearance
// fingerprint to it, and returns a lease. affinity (e.g. account identity)
// keeps the same account on the same exit so clearance stays valid. When no
// healthy node exists, or clearance cannot be resolved for a flare-solve node,
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
	fingerprint := m.fingerprint(*node, affinity)
	var ua, cookies string
	var version uint64
	if usesBrowserClearance(scope) {
		ua, cookies, version, err = m.resolveFingerprint(ctx, *node, fingerprint)
		if err != nil {
			return nil, err
		}
	}

	// Isolate connection pools by node, proxy URL, and clearance binding. The
	// proxy component is hashed so credentials never appear in cache keys or
	// diagnostics, while a same-name node whose URL changes gets a fresh pool.
	proxyFunc, proxyKey := egresspool.ProxyFunc(proxy)
	poolKey := "egress:" + node.Name + "|proxy=" + egresspool.ShortHash(proxyKey) + "|" + fingerprint
	// The ClientHello follows the UA this exit will send, so the TLS fingerprint
	// and the advertised Chrome version cannot contradict each other.
	client := httpclient.GetSharedBrowserHTTPClientForUserAgent(poolKey, m.config().GrokWebRequestTimeout(), 0, proxyFunc, ua)

	return &Lease{
		NodeID:           node.Name,
		ProxyURL:         proxyKey,
		UserAgent:        ua,
		CFCookies:        cookies,
		clearanceKey:     fingerprint,
		clearanceVersion: version,
		client:           client,
		manager:          m,
	}, nil
}

func (m *Manager) fingerprint(node Node, affinity string) string {
	cfg := m.clearanceConfig()
	return egresspool.ShortHash(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(node.Name)),
		strings.TrimSpace(node.URL),
		strings.TrimRight(strings.TrimSpace(cfg.FlareSolverrURL), "/"),
		strings.TrimRight(strings.TrimSpace(cfg.TargetURL), "/"),
		strings.ToLower(strings.TrimSpace(affinity)),
	}, "\x00"))
}

func usesBrowserClearance(scope string) bool {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "cli", "build":
		return false
	default:
		return true
	}
}

// probeWebNode checks a node against grok.com with a browser client and the
// node's stable UA, so a UA-based block is detected too.
func probeWebNode(ctx context.Context, cfg *config.Config, node Node) error {
	target := clearanceConfigFor(cfg).TargetURL
	if strings.TrimSpace(target) == "" {
		target = "https://grok.com/"
	}
	proxy, err := node.ProxyFor("probe")
	if err != nil {
		return err
	}
	proxyFunc, _ := egresspool.ProxyFunc(proxy)
	client := httpclient.GetSharedBrowserHTTPClientWithHeaderTimeout(
		"egress-probe:"+node.Name+"|proxy="+egresspool.ShortHash(node.URL),
		egresspool.ProbeTimeout, 0, proxyFunc)
	return egresspool.ProbeThrough(ctx, client, target, pickUserAgent(node.Name))
}

// Lease is a pinned egress path for one request: a node, its proxy URL, the
// browser User-Agent and Cloudflare cookies bound to that node/fingerprint, and
// the clearance key/version so a confirmed challenge can invalidate exactly the
// clearance this request used.
type Lease struct {
	Direct           bool // the scope fell back to the general proxy
	NodeID           string
	ProxyURL         string
	UserAgent        string
	CFCookies        string
	clearanceKey     string
	clearanceVersion uint64
	client           *http.Client
	manager          *Manager
}

// Do issues the request through the lease's client (proxy + UA + cookies are
// injected at request time by the client's RoundTripper or the caller).
func (l *Lease) Do(req *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errNoClient
	}
	return l.client.Do(req)
}

// InvalidateClearance marks exactly the clearance binding this lease used as
// stale. Concurrent re-solves bump the clearance version, so a stale
// invalidation can never delete a newer clearance.
func (l *Lease) InvalidateClearance() {
	if l == nil || l.manager == nil || l.clearanceKey == "" {
		return
	}
	l.manager.invalidateClearanceKey(l.clearanceKey, l.clearanceVersion)
}

// Release ends the lease. The HTTP client is shared process-wide, so a lease
// owns no connection pool of its own.
func (l *Lease) Release() {}
