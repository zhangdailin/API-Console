package middleware

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type clientIPContextKey struct{}

// TrustedProxyMiddleware accepts forwarding headers only from explicitly
// configured proxy addresses. Entries may be individual IPs or CIDR ranges.
// With an empty list every forwarding header is discarded (safe default).
func TrustedProxyMiddleware(values []string) (func(http.Handler) http.Handler, error) {
	networks, err := parseTrustedProxyNetworks(values)
	if err != nil {
		return nil, err
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			remote := remoteIP(r.RemoteAddr)
			if remote == nil || !ipInNetworks(remote, networks) {
				clearForwardingHeaders(r.Header)
				ctx := context.WithValue(r.Context(), clientIPContextKey{}, ipString(remote, r.RemoteAddr))
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}

			// Cloudflare's CF-Connecting-IP names the original client and is
			// authoritative *only* because the peer is trusted (Caddy on loopback, or
			// a Cloudflare edge the deployment listed). The X-Forwarded-For walk is
			// the fallback, and the peer itself the last resort.
			client := cloudflareClientIP(remote, r.Header.Get("CF-Connecting-IP"))
			if client == nil {
				client = forwardedClientIP(remote, r.Header.Get("X-Forwarded-For"), networks)
			}
			if client == nil {
				client = remote
			}
			// Downstream code receives only the resolved client, never the raw
			// attacker-controlled chain. Forwarded is removed because this
			// middleware intentionally implements only X-Forwarded-* semantics.
			r.Header.Del("Forwarded")
			r.Header.Set("X-Forwarded-For", client.String())
			sanitizeForwardedSingleValue(r.Header, "X-Forwarded-Host", nil)
			sanitizeForwardedSingleValue(r.Header, "X-Forwarded-Proto", func(value string) bool {
				return strings.EqualFold(value, "http") || strings.EqualFold(value, "https")
			})
			r.Header.Set("X-Real-IP", client.String())
			ctx := context.WithValue(r.Context(), clientIPContextKey{}, client.String())
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}, nil
}

func sanitizeForwardedSingleValue(header http.Header, name string, valid func(string) bool) {
	parts := strings.Split(header.Get(name), ",")
	value := strings.TrimSpace(parts[len(parts)-1])
	if value == "" || (valid != nil && !valid(value)) {
		header.Del(name)
		return
	}
	header.Set(name, value)
}

// ClientIP returns the trusted-proxy-aware caller IP captured at ingress.
func ClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if value, _ := r.Context().Value(clientIPContextKey{}).(string); value != "" {
		return value
	}
	return ipString(remoteIP(r.RemoteAddr), r.RemoteAddr)
}

func parseTrustedProxyNetworks(values []string) ([]*net.IPNet, error) {
	networks := make([]*net.IPNet, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if strings.Contains(value, "/") {
			_, network, err := net.ParseCIDR(value)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", value, err)
			}
			networks = append(networks, network)
			continue
		}
		ip := net.ParseIP(value)
		if ip == nil {
			return nil, fmt.Errorf("invalid trusted proxy IP %q", value)
		}
		bits := 128
		if ip.To4() != nil {
			ip = ip.To4()
			bits = 32
		}
		networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return networks, nil
}

func forwardedClientIP(remote net.IP, header string, trusted []*net.IPNet) net.IP {
	chain := make([]net.IP, 0, 8)
	for _, raw := range strings.Split(header, ",") {
		if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
			chain = append(chain, ip)
		}
	}
	current := remote
	for i := len(chain) - 1; i >= 0 && ipInNetworks(current, trusted); i-- {
		current = chain[i]
	}
	return current
}

func remoteIP(value string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(value), "[]")
	}
	return net.ParseIP(host)
}

func ipInNetworks(ip net.IP, networks []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network != nil && network.Contains(ip) {
			return true
		}
	}
	return false
}

func clearForwardingHeaders(header http.Header) {
	for _, name := range []string{
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP",
		// A direct caller must not be able to name itself through the header the
		// trusted path trusts; only a trusted peer's value survives.
		"CF-Connecting-IP",
	} {
		header.Del(name)
	}
}

// cloudflareClientIP reads Cloudflare's own client header. It returns nil when the
// header is absent or unparsable, so the caller falls back to the forwarded chain.
func cloudflareClientIP(remote net.IP, value string) net.IP {
	if remote == nil {
		return nil
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	// Cloudflare sends a single address; a comma means something else wrote here.
	if strings.Contains(trimmed, ",") {
		return nil
	}
	return net.ParseIP(trimmed)
}

func ipString(ip net.IP, fallback string) string {
	if ip != nil {
		return ip.String()
	}
	return strings.TrimSpace(fallback)
}
