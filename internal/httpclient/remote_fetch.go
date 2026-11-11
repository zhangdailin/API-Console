package httpclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// This file guards the one place where the gateway itself dials a URL that a
// client supplied: fetching a remote image_url / attachment into a data URI.
// Without it the proxy is a generic SSRF primitive — a caller could point it at
// cloud metadata (169.254.169.254), loopback admin ports or private networks.
//
// grok2api enforces the same property in its web provider: https on port 443,
// no userinfo, every resolved address must be public, and the connection is
// pinned to the validated address so DNS cannot be re-pointed between the check
// and the dial.

const (
	maxRemoteFetchRedirects = 3
	remoteFetchDialTimeout  = 15 * time.Second
)

var ErrRemoteFetchBlocked = errors.New("remote url is not allowed")

// ValidateRemoteFetchURL rejects URLs that can never be a legitimate remote
// media reference: credentials in the URL, non-HTTP schemes, empty hosts.
func ValidateRemoteFetchURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty url", ErrRemoteFetchBlocked)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRemoteFetchBlocked, err)
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("%w: url must not carry credentials", ErrRemoteFetchBlocked)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrRemoteFetchBlocked, parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return nil, fmt.Errorf("%w: url host is required", ErrRemoteFetchBlocked)
	}
	return parsed, nil
}

// IsPublicAddr reports whether an address may be dialled on behalf of a client.
// It rejects loopback, private, link-local, multicast, unspecified, CGNAT and
// reserved ranges so an internal service can never be reached by reflection.
func IsPublicAddr(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsPrivate() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsInterfaceLocalMulticast() {
		return false
	}
	if addr.Is4() {
		b := addr.As4()
		switch {
		case b[0] == 0: // 0.0.0.0/8
			return false
		case b[0] == 100 && b[1] >= 64 && b[1] <= 127: // 100.64.0.0/10 CGNAT
			return false
		case b[0] == 192 && b[1] == 0 && b[2] == 0: // 192.0.0.0/24
			return false
		case b[0] == 198 && (b[1] == 18 || b[1] == 19): // 198.18.0.0/15 benchmarking
			return false
		case b[0] >= 240: // 240.0.0.0/4 reserved, includes broadcast
			return false
		}
	}
	return true
}

// ResolvePublicAddrs resolves host and returns its addresses, refusing when any
// resolved address is not public. Every address is checked (not just the first)
// so a DNS answer that mixes a public and a private record cannot smuggle the
// private one through a retry.
func ResolvePublicAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(strings.TrimSpace(host)); err == nil {
		if !IsPublicAddr(literal) {
			return nil, fmt.Errorf("%w: address %s is not public", ErrRemoteFetchBlocked, literal)
		}
		return []netip.Addr{literal}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%w: %s has no address", ErrRemoteFetchBlocked, host)
	}
	public := make([]netip.Addr, 0, len(addrs))
	for _, addr := range addrs {
		if !IsPublicAddr(addr) {
			return nil, fmt.Errorf("%w: %s resolves to non-public %s", ErrRemoteFetchBlocked, host, addr)
		}
		public = append(public, addr)
	}
	return public, nil
}

// PublicOnlyDialContext dials only validated public addresses, pinning the
// connection to the resolved IP so a second DNS lookup (rebinding) cannot move
// the peer after the check.
func PublicOnlyDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := ResolvePublicAddrs(ctx, host)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: remoteFetchDialTimeout}
	var lastErr error
	for _, addr := range addrs {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%w: no dialable address for %s", ErrRemoteFetchBlocked, host)
	}
	return nil, lastErr
}

// NewRemoteFetchClient builds the HTTP client used for client-supplied media
// URLs. When an operator proxy is configured the transport cannot pin the peer
// address (the proxy performs the dial), so the target is resolved and
// validated up front and again on every redirect hop.
func NewRemoteFetchClient(timeout time.Duration, proxyFunc func(*http.Request) (*url.URL, error)) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	proxied := proxyFunc != nil
	if proxied {
		transport.Proxy = proxyFunc
	} else {
		transport.Proxy = nil
		transport.DialContext = PublicOnlyDialContext
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRemoteFetchRedirects {
				return fmt.Errorf("%w: too many redirects", ErrRemoteFetchBlocked)
			}
			target, err := ValidateRemoteFetchURL(req.URL.String())
			if err != nil {
				return err
			}
			if proxied {
				ctx, cancel := context.WithTimeout(req.Context(), remoteFetchDialTimeout)
				defer cancel()
				if _, err := ResolvePublicAddrs(ctx, target.Hostname()); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// CheckRemoteFetchTarget performs the up-front validation shared by every
// caller before the request is built.
func CheckRemoteFetchTarget(ctx context.Context, rawURL string, proxied bool) (*url.URL, error) {
	target, err := ValidateRemoteFetchURL(rawURL)
	if err != nil {
		return nil, err
	}
	// Resolve and validate on every path. A direct dial is additionally pinned
	// to the validated address, but the up-front check must stand on its own:
	// otherwise a caller that inspects the error cannot tell a blocked target
	// from an allowed one until the connection is attempted.
	if _, err := ResolvePublicAddrs(ctx, target.Hostname()); err != nil {
		return nil, err
	}
	return target, nil
}
