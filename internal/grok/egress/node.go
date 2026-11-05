package egress

import (
	"net/http"
	"net/url"
	"sync/atomic"

	"orchids-api/internal/grok/egressnodes"
)

// Node is one egress exit for the Grok proxy pool. URL empty means direct.
// Name is the registry node ID, the stable key for health and stickiness.
type Node struct {
	Name    string
	Label   string   // operator-facing name
	URL     string   // proxy address as configured; may hold {account}
	Proxy   *url.URL // parsed proxy URL when it needs no substitution
	Weight  int      // weight for weighted round-robin; <=0 = 1
	Scope   string   // registry scope (grok_build, grok_web, grok_web_asset)
	Proxied bool
}

func nodeFromRegistry(node egressnodes.Node) Node {
	converted := Node{Name: node.ID, Label: node.Name, URL: node.ProxyURL, Weight: 1, Scope: node.Scope, Proxied: !node.Direct()}
	if proxy, err := node.ProxyFor(""); err == nil && !node.Pool {
		converted.Proxy = proxy
	}
	return converted
}

// proxyFor resolves the node's proxy for an account affinity.
func (n Node) proxyFor(affinity string) (*url.URL, error) {
	return egressnodes.Node{ProxyURL: n.URL}.ProxyFor(affinity)
}

// nodeMatchesScope reports whether a node serves a request scope.
func nodeMatchesScope(node Node, scope string) bool {
	return node.Scope == egressnodes.RegistryScope(scope)
}

// parseProxyURL validates and parses a proxy URL; empty means direct.
func parseProxyURL(raw string) (*url.URL, error) { return egressnodes.ParseProxyURL(raw) }

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

func proxyFuncForNode(node Node) func(*http.Request) (*url.URL, error) {
	if node.Proxy == nil {
		return nil
	}
	return http.ProxyURL(node.Proxy)
}
