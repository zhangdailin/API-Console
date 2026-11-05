package egress

import (
	"net/http"
	"net/url"

	"orchids-api/internal/grok/egressnodes"
)

// Node is one egress exit for the Grok proxy pool. URL empty means direct.
// Name is the registry node ID, the stable key for health, stickiness and
// clearance.
type Node struct {
	Name      string
	Label     string
	URL       string   // proxy address as configured; may hold {account}
	Proxy     *url.URL // parsed proxy URL when it needs no substitution
	Weight    int
	Scope     string // registry scope
	Proxied   bool
	UserAgent string // manual clearance: the UA the cookies were issued to
	Cookies   string // manual clearance cookies
}

func nodeFromRegistry(node egressnodes.Node) Node {
	converted := Node{Name: node.ID, Label: node.Name, URL: node.ProxyURL, Weight: 1, Scope: node.Scope, Proxied: !node.Direct(), UserAgent: node.UserAgent, Cookies: node.Cookies}
	if proxy, err := node.ProxyFor(""); err == nil && !node.Pool {
		converted.Proxy = proxy
	}
	return converted
}

func (n Node) proxyFor(affinity string) (*url.URL, error) {
	return egressnodes.Node{ProxyURL: n.URL}.ProxyFor(affinity)
}

// parseProxyURL validates and parses a proxy URL; empty means direct.
func parseProxyURL(raw string) (*url.URL, error) { return egressnodes.ParseProxyURL(raw) }

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
	release          func()
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

// Release returns the lease's underlying client to the pool.
func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

func proxyFuncForNode(node Node) func(*http.Request) (*url.URL, error) {
	if node.Proxy == nil {
		return nil
	}
	return http.ProxyURL(node.Proxy)
}
