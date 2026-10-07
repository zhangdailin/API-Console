package config

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ParseProxyURL parses a configured proxy URL and rejects unsupported schemes
// or missing hosts instead of silently turning them into a direct connection.
func ParseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		// url.Error includes the original URL, which can carry credentials.
		return nil, fmt.Errorf("invalid proxy URL")
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return nil, fmt.Errorf("proxy URL missing host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("proxy port must be between 1 and 65535")
		}
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
	}
}

// ValidateProxyConfig checks every configured proxy field before it is loaded
// or persisted. Empty fields are valid; a non-empty malformed field is not.
func ValidateProxyConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	fields := []struct {
		name string
		raw  string
	}{
		{"proxy_url", cfg.ProxyURL},
		{"proxy_http", cfg.ProxyHTTP},
		{"proxy_https", cfg.ProxyHTTPS},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.raw) == "" {
			continue
		}
		if _, err := ParseProxyURL(field.raw); err != nil {
			return fmt.Errorf("%s: %w", field.name, err)
		}
	}
	return nil
}
