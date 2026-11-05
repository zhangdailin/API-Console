package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/egressnodes"
	"orchids-api/internal/testutil"
)

func TestNodesFromConfigDisabled(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: false})
	testutil.False(t, m.Routes("cli"), "without nodes Build keeps the general proxy")
}

func TestManagerAcquireDisabled(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: false})
	lease, err := m.Acquire(context.Background(), "cli", "acct")
	testutil.NoError(t, err)
	testutil.True(t, lease.Direct, "a scope without nodes falls back to the general proxy")
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
	for i := range 10 {
		lease, err := m.Acquire(context.Background(), "cli", "acct-3")
		testutil.Falsef(t, err != nil, "acquire %d failed: %v", i, err)
		testutil.NotEqual(t, lease.NodeID, "bad")
		lease.Release()
	}
}

func TestParseProxyURL(t *testing.T) {
	valid := []string{
		"http://proxy1:8080",
		"socks5://user:pass@proxy1:1080",
		"socks5h://proxy1:1080",
	}
	for _, raw := range valid {
		_, err := egressnodes.ParseProxyURL(raw)
		testutil.CheckNoError(t, err)
	}
	invalid := []string{
		"https://proxy1:8443", // https proxy unsupported by the browser transport
		"socks4://proxy1:1080",
		"://missing-scheme",
		"http://", // no host
		"http://host:notaport",
	}
	for _, raw := range invalid {
		_, err := egressnodes.ParseProxyURL(raw)
		testutil.CheckError(t, err)
	}
}

func TestAcquireFailsClosedWhenNoNodes(t *testing.T) {
	m := NewManager(&config.Config{GrokEgressEnabled: true})
	testutil.False(t, m == nil, "expected manager for enabled egress")
	_, err := m.Acquire(context.Background(), "cli", "acct")
	testutil.Error(t, err)
}
