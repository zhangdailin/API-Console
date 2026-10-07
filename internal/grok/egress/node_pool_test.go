package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestNodesFromConfigDisabled(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: false}
	nodes := nodesFromConfig(cfg)
	testutil.Falsef(t, len(nodes) != 0, "disabled egress should yield no nodes, got %d", len(nodes))
}

func TestNodesFromConfig(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes: []config.EgressNodeConfig{
			{Name: "a", URL: "http://proxy1:8080", Weight: 2, Scope: "cli"},
			{Name: "b", URL: "", Scope: "all"}, // direct
		},
	}
	nodes := nodesFromConfig(cfg)
	testutil.Equal(t, len(nodes), 2)
	testutil.Equal(t, nodes[0].Weight, 2)
	testutil.False(t, !nodes[0].Proxied, "node a should be proxied")
	testutil.False(t, nodes[1].Proxied, "node b should be direct")
}

func TestNodeMatchesScope(t *testing.T) {
	node := Node{Name: "n", Scope: "cli"}
	testutil.False(t, !nodeMatchesScope(node, "cli"), "cli node should match cli scope")
	testutil.False(t, nodeMatchesScope(node, "other"), "cli node should not match another scope")
	all := Node{Name: "n", Scope: "all"}
	testutil.False(t, !nodeMatchesScope(all, "cli") || !nodeMatchesScope(all, "other"), "all scope should match any scope")
}

func TestManagerAcquireDisabled(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: false}
	m := NewManager(cfg)
	testutil.False(t, m != nil, "disabled egress should yield nil manager")
}

func TestManagerAcquireDirectNode(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes:   []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
	}
	m := NewManager(cfg)
	testutil.False(t, m == nil, "expected manager")
	lease, err := m.Acquire(context.Background(), "cli", "acct-1")
	testutil.NoError(t, err, "acquire failed: %v")
	defer lease.Release()
}

func TestLeaseReleasePreventsReuse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	m := NewManager(&config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes:   []config.EgressNodeConfig{{Name: "direct", Scope: "all"}},
	})
	lease, err := m.Acquire(context.Background(), "cli", "acct-release")
	testutil.NoError(t, err)
	lease.Release()
	lease.Release()
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	testutil.NoError(t, err)
	_, err = lease.Do(req)
	testutil.True(t, errors.Is(err, errLeaseReleased), "released lease must reject a valid request")
	other, err := m.Acquire(context.Background(), "cli", "acct-other")
	testutil.NoError(t, err)
	defer other.Release()
	resp, err := other.Do(req)
	testutil.NoError(t, err, "releasing one lease must not close the shared client")
	defer resp.Body.Close()
	testutil.Equal(t, resp.StatusCode, http.StatusNoContent)
}

func TestManagerUnhealthyNodeSkipped(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes: []config.EgressNodeConfig{
			{Name: "bad", Scope: "cli"},
			{Name: "good", Scope: "cli"},
		},
	}
	m := NewManager(cfg)
	m.FeedbackOutcome("bad", OutcomeServerError)
	for i := 0; i < 10; i++ {
		lease, err := m.Acquire(context.Background(), "cli", "acct-3")
		testutil.Falsef(t, err != nil, "acquire %d failed: %v", i, err)
		testutil.NotEqual(t, lease.NodeID, "bad")
		lease.Release()
	}
}

func TestFeedbackHealth(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: true}
	m := NewManager(cfg)
	m.FeedbackOutcome("n1", OutcomeSuccess)
	m.mu.RLock()
	score := m.health["n1"]
	m.mu.RUnlock()
	testutil.Falsef(t, score <= 0, "expected positive health after success, got %f", score)
	m.FeedbackOutcome("n1", OutcomeServerError)
	m.mu.RLock()
	scoreAfter := m.health["n1"]
	m.mu.RUnlock()
	testutil.Falsef(t, scoreAfter >= score, "expected health to drop after failure: before=%f after=%f", score, scoreAfter)
}

func TestFeedbackOutcome_RateLimitKeepsHealth(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: true})
	m.FeedbackOutcome("n1", OutcomeSuccess)
	m.mu.RLock()
	before := m.health["n1"]
	m.mu.RUnlock()
	m.FeedbackOutcome("n1", OutcomeRateLimited)
	m.mu.RLock()
	after := m.health["n1"]
	m.mu.RUnlock()
	testutil.Equal(t, after, before)
}

func TestParseProxyURL(t *testing.T) {
	valid := []string{
		"http://proxy1:8080",
		"socks5://user:pass@proxy1:1080",
		"socks5h://proxy1:1080",
	}
	for _, raw := range valid {
		_, err := parseProxyURL(raw)
		testutil.CheckNoError(t, err)
	}
	invalid := []string{
		"https://proxy1:8443", // https proxy unsupported by the browser transport
		"socks4://proxy1:1080",
		"trojan://secret@host:443",
		"://missing-scheme",
		"http://", // no host
		"http://host:notaport",
	}
	for _, raw := range invalid {
		_, err := parseProxyURL(raw)
		testutil.CheckError(t, err)
	}
}

func TestAcquireFailsClosedWhenNoNodes(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: true})
	testutil.False(t, m == nil, "expected manager for enabled egress")
	_, err := m.Acquire(context.Background(), "cli", "acct")
	testutil.Error(t, err)
}

func TestNodeCooldownGrowsAndCaps(t *testing.T) {
	cases := map[int]time.Duration{
		0:  nodeCooldown,
		1:  nodeCooldown,
		2:  2 * nodeCooldown,
		3:  4 * nodeCooldown,
		4:  8 * nodeCooldown,
		5:  16 * nodeCooldown,
		6:  nodeCooldownMax,
		20: nodeCooldownMax,
	}
	for failures, want := range cases {
		testutil.Equal(t, nodeCooldownFor(failures), want)
		got := nodeCooldownFor(failures)
		testutil.Falsef(t, got > nodeCooldownMax, "nodeCooldownFor(%d) = %s exceeds the cap", failures, got)
	}
}

func TestFeedbackOutcomeBacksOffExponentially(t *testing.T) {
	m := &Manager{
		cfg:       &config.Config{GrokEgressEnabled: true},
		nodes:     []Node{{Name: "n1", Scope: "all", Weight: 1}},
		health:    map[string]float64{},
		unhealthy: map[string]time.Time{},
		failures:  map[string]int{},
		lastError: map[string]string{},
		lastProbe: map[string]time.Time{},
	}
	m.FeedbackOutcome("n1", OutcomeTransportError)
	first := time.Until(m.unhealthy["n1"])
	testutil.Falsef(t, first > nodeCooldown+2*time.Second || first < nodeCooldown-2*time.Second, "first cooldown = %s, want about %s", first, nodeCooldown)
	m.FeedbackOutcome("n1", OutcomeTransportError)
	second := time.Until(m.unhealthy["n1"])
	testutil.Falsef(t, second < first, "second cooldown %s must be longer than the first %s", second, first)
	testutil.Equal(t, m.lastError["n1"], "transport")
	// A success clears both the cooldown and the accumulated backoff.
	m.FeedbackOutcome("n1", OutcomeSuccess)
	testutil.Equal(t, m.failures["n1"], 0)
	_, cooling := m.unhealthy["n1"]
	testutil.False(t, cooling, "a successful node must leave the cooldown map")
}

func TestHealthPersistsAcrossManagers(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{GrokEgressEnabled: true, MediaDir: dir, GrokEgressNodes: []config.EgressNodeConfig{
		{Name: "n1", URL: "http://127.0.0.1:1", Scope: "all"},
	}}
	first := NewManager(cfg)
	testutil.False(t, first == nil, "manager must be created when egress is enabled")
	first.FeedbackOutcome("n1", OutcomeTransportError)
	first.FeedbackOutcome("n1", OutcomeTransportError)
	// The write is throttled; force a flush by clearing the throttle.
	first.mu.Lock()
	first.lastPersist = time.Time{}
	first.persistHealthLocked()
	first.mu.Unlock()

	second := NewManager(cfg)
	testutil.False(t, second == nil, "second manager must be created")
	second.mu.RLock()
	failures := second.failures["n1"]
	_, cooling := second.unhealthy["n1"]
	second.mu.RUnlock()
	testutil.Equal(t, failures, 2)
	testutil.True(t, cooling, "restored node must keep its cooldown")
	// A node that no longer exists in the configuration must not come back.
	cfg.GrokEgressNodes = []config.EgressNodeConfig{{Name: "other", URL: "http://127.0.0.1:1", Scope: "all"}}
	third := NewManager(cfg)
	third.mu.RLock()
	_, restored := third.failures["n1"]
	third.mu.RUnlock()
	testutil.False(t, restored, "health of a removed node must not be restored")
}
