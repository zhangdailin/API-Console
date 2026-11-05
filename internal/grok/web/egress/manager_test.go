package egress

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

type countingSolver struct{ calls atomic.Int32 }

func (s *countingSolver) Solve(context.Context, ClearanceConfig, string) (clearanceSolution, error) {
	n := s.calls.Add(1)
	return clearanceSolution{Cookies: "cf_clearance=solved-" + string(rune('0'+n)), UserAgent: "solver-ua"}, nil
}

func webConfig(mode string) *config.Config {
	return &config.Config{
		GrokEgressEnabled:     true,
		GrokEgressNodes:       []config.EgressNodeConfig{{Name: "web-direct", Scope: "all"}},
		GrokClearanceMode:     mode,
		GrokConfigCFClearance: "manual-token",
	}
}

func TestAcquireBindsManualClearance(t *testing.T) {
	m := NewManager(webConfig("manual"))
	lease, err := m.Acquire(context.Background(), "app_chat", "acct-1")
	testutil.NoError(t, err)
	defer lease.Release()
	testutil.False(t, lease.Direct, "a scope with a node must not fall back to direct")
	testutil.Equal(t, lease.CFCookies, "cf_clearance=manual-token")
	testutil.True(t, strings.Contains(lease.UserAgent, "Chrome/"), "manual clearance must bind a rotated browser UA")

	again, err := m.Acquire(context.Background(), "app_chat", "acct-1")
	testutil.NoError(t, err)
	testutil.Equal(t, again.UserAgent, lease.UserAgent)
	testutil.Equal(t, again.NodeID, lease.NodeID)
}

func TestSolvedClearanceIsCachedUntilInvalidated(t *testing.T) {
	m := NewManager(webConfig("on_demand"))
	solver := &countingSolver{}
	m.solver = solver

	first, err := m.Acquire(context.Background(), "app_chat", "acct-2")
	testutil.NoError(t, err)
	testutil.Equal(t, first.CFCookies, "cf_clearance=solved-1")
	testutil.Equal(t, first.UserAgent, "solver-ua")

	cached, err := m.Acquire(context.Background(), "app_chat", "acct-2")
	testutil.NoError(t, err)
	testutil.Equal(t, cached.CFCookies, first.CFCookies)
	testutil.Equal(t, int(solver.calls.Load()), 1)

	// A confirmed challenge invalidates exactly this binding: the next lease re-solves.
	first.InvalidateClearance()
	resolved, err := m.Acquire(context.Background(), "app_chat", "acct-2")
	testutil.NoError(t, err)
	testutil.Equal(t, resolved.CFCookies, "cf_clearance=solved-2")
	testutil.Equal(t, int(solver.calls.Load()), 2)

	// A stale lease cannot invalidate the newer clearance.
	first.InvalidateClearance()
	still, err := m.Acquire(context.Background(), "app_chat", "acct-2")
	testutil.NoError(t, err)
	testutil.Equal(t, still.CFCookies, "cf_clearance=solved-2")
	testutil.Equal(t, int(solver.calls.Load()), 2)
}

func TestBuildScopeSkipsClearance(t *testing.T) {
	m := NewManager(webConfig("on_demand"))
	solver := &countingSolver{}
	m.solver = solver
	lease, err := m.Acquire(context.Background(), "cli", "acct-3")
	testutil.NoError(t, err)
	testutil.Equal(t, lease.CFCookies, "")
	testutil.Equal(t, int(solver.calls.Load()), 0)
}

func TestInspectAndRefreshClearance(t *testing.T) {
	m := NewManager(webConfig("on_demand"))
	solver := &countingSolver{}
	m.solver = solver
	lease, err := m.Acquire(context.Background(), "app_chat", "acct-4")
	testutil.NoError(t, err)
	testutil.Equal(t, m.InspectClearance(lease.NodeID).Status, "ready")
	testutil.NoError(t, m.RefreshClearance(context.Background(), lease.NodeID))
	testutil.Equal(t, int(solver.calls.Load()), 2)
	testutil.Error(t, m.RefreshClearance(context.Background(), "no-such-node"))
}
