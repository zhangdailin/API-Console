package egress

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"orchids-api/internal/config"
)

// Node is one egress exit for the Grok proxy pool. URL empty means direct.
type Node struct {
	Name    string
	URL     string   // proxy address as configured; empty = direct
	Proxy   *url.URL // parsed proxy URL; nil = direct
	Weight  int      // weight for weighted round-robin; <=0 = 1
	Scope   string   // "app_chat"|"console"|"cli"|"all"
	Proxied bool
}

// supportedProxySchemes mirrors the transport actually used by the browser
// client (httpclient.GetSharedBrowserHTTPClient): standard HTTP CONNECT and SOCKS5.
// HTTPS proxies, trojan/vless/ss/vmess and socks4 are rejected up-front so a
// misconfigured node fails loudly instead of failing per request.
var supportedProxySchemes = map[string]bool{
	"http": true, "socks5": true, "socks5h": true,
}

// parseProxyURL validates and parses a configured proxy URL. An empty string
// yields a nil (direct) proxy. It rejects unknown schemes and missing hosts so
// a misconfigured node fails loudly instead of failing per request.
func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if !supportedProxySchemes[strings.ToLower(parsed.Scheme)] {
		return nil, fmt.Errorf("unsupported proxy scheme %q (supported: http, socks5, socks5h)", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Host) == "" {
		return nil, errors.New("proxy URL missing host")
	}
	if err := validateProxyHost(parsed.Host); err != nil {
		return nil, err
	}
	return parsed, nil
}

// validateProxyHost checks a host[:port] for a numeric port when one is given.
func validateProxyHost(host string) error {
	if host == "" {
		return errors.New("proxy URL missing host")
	}
	if !strings.Contains(host, ":") {
		return nil
	}
	_, port, err := net.SplitHostPort(host)
	if err != nil {
		return fmt.Errorf("invalid proxy host %q: %w", host, err)
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid proxy port %q", port)
		}
	}
	return nil
}

func nodesFromConfig(cfg *config.Config) []Node {
	if cfg == nil || !cfg.GrokEgressEnabled {
		return nil
	}
	out := make([]Node, 0, len(cfg.GrokEgressNodes))
	for _, n := range cfg.GrokEgressNodes {
		scope := strings.ToLower(strings.TrimSpace(n.Scope))
		if scope == "" {
			scope = "all"
		}
		weight := n.Weight
		if weight <= 0 {
			weight = 1
		}
		proxy, err := parseProxyURL(n.URL)
		if err != nil {
			// Fail closed: a node that cannot be validated must not silently
			// route direct. Skip it; if the pool ends up empty Acquire errors.
			// The configured URL usually carries proxy credentials, so it must
			// not reach the log stream verbatim. The validation error itself
			// only names the offending scheme/host, never the userinfo.
			slog.Warn("egress node skipped: invalid proxy URL", "node", strings.TrimSpace(n.Name), "url", sanitizeFlareSolverrMessage(n.URL), "error", err)
			continue
		}
		out = append(out, Node{
			Name:    strings.TrimSpace(n.Name),
			URL:     strings.TrimSpace(n.URL),
			Proxy:   proxy,
			Weight:  weight,
			Scope:   scope,
			Proxied: n.Proxied || strings.TrimSpace(n.URL) != "",
		})
	}
	return out
}

func nodeMatchesScope(node Node, scope string) bool {
	nodeScope := strings.ToLower(strings.TrimSpace(node.Scope))
	scope = strings.ToLower(strings.TrimSpace(scope))
	if nodeScope == "" || nodeScope == "all" {
		return true
	}
	return nodeScope == scope
}

// Lease is a pinned egress path for one request: a node, its proxy URL, the
// browser User-Agent and Cloudflare cookies bound to that node/fingerprint, and
// the clearance key/version so a confirmed challenge can invalidate exactly the
// clearance this request used.
type Lease struct {
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
