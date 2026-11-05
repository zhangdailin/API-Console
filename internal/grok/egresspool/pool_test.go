package egresspool

import (
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

// testProfile is a plane profile for pool tests; Probe is never reached.
var testProfile = Profile{Namespace: "egresspool_test", HealthFile: ".egress-health-test.json"}

func newTestPool(cfg *config.Config) *Pool { return New(cfg, testProfile) }

func TestNodesFromConfig(t *testing.T) {
	cfg := &config.Config{
		GrokEgressEnabled: true,
		GrokEgressNodes: []config.EgressNodeConfig{
			{Name: "a", URL: "http://proxy1:8080", Scope: "cli"},
			{Name: "b", URL: "", Scope: "all"}, // direct; serves Build and Web
		},
	}
	m := newTestPool(cfg)
	var build []Node
	for _, node := range m.nodes {
		if node.Scope == m.targetScopeLocked("cli") {
			build = append(build, node)
		}
	}
	testutil.Equal(t, len(build), 2)
	testutil.Equal(t, len(m.nodes), 3)
	testutil.True(t, build[0].Proxied, "node a should be proxied")
	testutil.False(t, build[1].Proxied, "node b should be direct")
}

func TestTargetScope(t *testing.T) {
	m := newTestPool(&config.Config{GrokEgressEnabled: true, GrokEgressNodes: []config.EgressNodeConfig{
		{Name: "web", Scope: "app_chat"},
	}})
	testutil.Equal(t, m.targetScopeLocked("cli"), "grok_build")
	testutil.NotEqual(t, m.targetScopeLocked("app_chat"), "grok_build")
	// Without asset nodes, Web asset downloads use the Web nodes.
	testutil.Equal(t, m.targetScopeLocked("asset"), m.targetScopeLocked("app_chat"))
}

func TestFeedbackHealth(t *testing.T) {
	cfg := &config.Config{GrokEgressEnabled: true}
	m := newTestPool(cfg)
	m.Feedback("n1", OutcomeSuccess)
	m.mu.RLock()
	score := m.health["n1"]
	m.mu.RUnlock()
	testutil.Falsef(t, score <= 0, "expected positive health after success, got %f", score)
	m.Feedback("n1", OutcomeServerError)
	m.mu.RLock()
	scoreAfter := m.health["n1"]
	m.mu.RUnlock()
	testutil.Falsef(t, scoreAfter >= score, "expected health to drop after failure: before=%f after=%f", score, scoreAfter)
}

func TestFeedbackOutcome_RateLimitKeepsHealth(t *testing.T) {
	m := newTestPool(&config.Config{GrokEgressEnabled: true})
	m.Feedback("n1", OutcomeSuccess)
	m.mu.RLock()
	before := m.health["n1"]
	m.mu.RUnlock()
	m.Feedback("n1", OutcomeRateLimited)
	m.mu.RLock()
	after := m.health["n1"]
	m.mu.RUnlock()
	testutil.Equal(t, after, before)
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
	m := &Pool{
		cfg:       &config.Config{GrokEgressEnabled: true},
		metrics:   metricsFor(testProfile.Namespace),
		nodes:     []Node{{Name: "n1", Scope: "all", Weight: 1}},
		health:    map[string]float64{},
		unhealthy: map[string]time.Time{},
		failures:  map[string]int{},
		lastError: map[string]string{},
		lastProbe: map[string]time.Time{},
	}
	m.Feedback("n1", OutcomeTransportError)
	first := time.Until(m.unhealthy["n1"])
	testutil.Falsef(t, first > nodeCooldown+2*time.Second || first < nodeCooldown-2*time.Second, "first cooldown = %s, want about %s", first, nodeCooldown)
	m.Feedback("n1", OutcomeTransportError)
	second := time.Until(m.unhealthy["n1"])
	testutil.Falsef(t, second < first, "second cooldown %s must be longer than the first %s", second, first)
	testutil.Equal(t, m.lastError["n1"], "transport")
	// A success clears both the cooldown and the accumulated backoff.
	m.Feedback("n1", OutcomeSuccess)
	testutil.Equal(t, m.failures["n1"], 0)
	_, cooling := m.unhealthy["n1"]
	testutil.False(t, cooling, "a successful node must leave the cooldown map")
}

func TestHealthPersistsAcrossManagers(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{GrokEgressEnabled: true, MediaDir: dir, GrokEgressNodes: []config.EgressNodeConfig{
		{Name: "n1", URL: "http://127.0.0.1:1", Scope: "all"},
	}}
	first := newTestPool(cfg)
	testutil.False(t, first == nil, "manager must be created when egress is enabled")
	first.Feedback("n1", OutcomeTransportError)
	first.Feedback("n1", OutcomeTransportError)
	// The write is throttled; force a flush by clearing the throttle.
	first.mu.Lock()
	first.lastPersist = time.Time{}
	first.persistHealthLocked()
	first.mu.Unlock()

	second := newTestPool(cfg)
	testutil.False(t, second == nil, "second manager must be created")
	second.mu.RLock()
	failures := second.failures["n1"]
	_, cooling := second.unhealthy["n1"]
	second.mu.RUnlock()
	testutil.Equal(t, failures, 2)
	testutil.True(t, cooling, "restored node must keep its cooldown")
	// A node that no longer exists in the configuration must not come back.
	cfg.GrokEgressNodes = []config.EgressNodeConfig{{Name: "other", URL: "http://127.0.0.1:1", Scope: "all"}}
	third := newTestPool(cfg)
	third.mu.RLock()
	_, restored := third.failures["n1"]
	third.mu.RUnlock()
	testutil.False(t, restored, "health of a removed node must not be restored")
}
