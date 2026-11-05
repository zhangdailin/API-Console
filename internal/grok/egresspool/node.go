package egresspool

import (
	"net/http"
	"net/url"

	"orchids-api/internal/grok/egressnodes"
)

// Node is one egress exit. URL empty means direct. Name is the registry node
// ID, the stable key for health, stickiness and (Web) clearance.
type Node struct {
	Name      string
	Label     string   // operator-facing name
	URL       string   // proxy address as configured; may hold {account}
	Proxy     *url.URL // parsed proxy URL when it needs no substitution
	Weight    int      // weight for weighted round-robin
	Scope     string   // registry scope (grok_build, grok_web, grok_web_asset)
	Proxied   bool
	UserAgent string // Web manual clearance: the UA the cookies were issued to
	Cookies   string // Web manual clearance cookies
}

func nodeFromRegistry(node egressnodes.Node) Node {
	converted := Node{Name: node.ID, Label: node.Name, URL: node.ProxyURL, Weight: 1, Scope: node.Scope, Proxied: !node.Direct(), UserAgent: node.UserAgent, Cookies: node.Cookies}
	if proxy, err := node.ProxyFor(""); err == nil && !node.Pool {
		converted.Proxy = proxy
	}
	return converted
}

// ProxyFor resolves the node's proxy for an account affinity; nil means direct.
func (n Node) ProxyFor(affinity string) (*url.URL, error) {
	return egressnodes.Node{ProxyURL: n.URL}.ProxyFor(affinity)
}

// ProxyFunc returns the transport proxy function for a resolved proxy, and the
// proxy's string form for pool keys ("" when direct).
func ProxyFunc(proxy *url.URL) (func(*http.Request) (*url.URL, error), string) {
	if proxy == nil {
		return nil, ""
	}
	return http.ProxyURL(proxy), proxy.String()
}
