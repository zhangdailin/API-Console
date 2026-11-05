package egressnodes

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// directSchemes are proxy URLs the HTTP and browser transports dial
// themselves: HTTP CONNECT and SOCKS5.
var directSchemes = map[string]bool{"http": true, "socks5": true, "socks5h": true}

// tunnelSchemes are share-link protocols that need a local tunnel.
var tunnelSchemes = map[string]bool{"trojan": true, "vless": true, "ss": true, "vmess": true}

// IsTunnelURL reports whether raw is a tunnel share link.
func IsTunnelURL(raw string) bool {
	scheme, _, ok := strings.Cut(strings.TrimSpace(raw), "://")
	return ok && tunnelSchemes[strings.ToLower(scheme)]
}

// ValidateProxyURL accepts an empty (direct) address, a dialable proxy, or a
// tunnel share link the tunnel runtime can open.
func ValidateProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if IsTunnelURL(raw) {
		return validateTunnelURL(raw)
	}
	probe := strings.ReplaceAll(raw, "{account}", "account")
	_, err := ParseProxyURL(probe)
	return err
}

// ParseProxyURL parses a dialable proxy address. Tunnel links resolve to the
// local endpoint of their tunnel.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if IsTunnelURL(raw) {
		return tunnelEndpoint(raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("invalid proxy URL")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if !directSchemes[scheme] {
		return nil, fmt.Errorf("unsupported proxy scheme %q (supported: http, socks5, socks5h, trojan, vless, ss, vmess)", parsed.Scheme)
	}
	if strings.TrimSpace(parsed.Hostname()) == "" {
		return nil, errors.New("proxy URL missing host")
	}
	if port := parsed.Port(); port != "" {
		for _, r := range port {
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("invalid proxy port %q", port)
			}
		}
	}
	return parsed, nil
}

// MaskProxy renders a proxy address without credentials: scheme://host:port,
// with "***@" marking that credentials exist. Tunnel links show their server.
func MaskProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "***"
	}
	if IsTunnelURL(raw) {
		if info, err := parseTunnelURL(raw); err == nil {
			return strings.ToLower(scheme) + "://" + net.JoinHostPort(info.Host, info.Port)
		}
		return strings.ToLower(scheme) + "://***"
	}
	parsed, err := url.Parse(scheme + "://" + strings.ReplaceAll(rest, "{account}", "account"))
	if err != nil {
		return strings.ToLower(scheme) + "://***"
	}
	credentials := ""
	if parsed.User != nil {
		credentials = "***@"
	}
	return strings.ToLower(scheme) + "://" + credentials + parsed.Host
}

// Fingerprint is a short stable identifier of a proxy address, so operators
// can tell two masked addresses apart without seeing either.
func Fingerprint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return shortHash(raw)[:8]
}

// NewID returns a random record identifier.
func NewID() string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
