package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// statsigSignerMaxURL bounds a configured signer URL.
const statsigSignerMaxURL = 2048

// ValidateStatsigSignerURL accepts a public HTTPS endpoint on 443, or an
// explicitly configured internal address. A signer URL arrives from configuration,
// and this gateway calls it with account-derived metadata, so a public plaintext
// or custom-port endpoint is refused rather than silently trusted. The admin
// plane checks it on save; the Web client checks it again before each call.
func ValidateStatsigSignerURL(value string) error {
	raw := strings.TrimSpace(value)
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(raw, "#") || len(raw) > statsigSignerMaxURL {
		return fmt.Errorf("statsig signer URL must be a complete address without credentials, query or fragment")
	}
	if port := parsed.Port(); port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || number < 1 || number > 65535 {
			return fmt.Errorf("statsig signer URL has an invalid port")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	internal := statsigInternalHost(host)
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		if internal {
			return nil
		}
	case "https":
		if internal || parsed.Port() == "" || parsed.Port() == "443" {
			return nil
		}
	}
	return fmt.Errorf("a public statsig signer URL must use https on 443; http and custom ports are only allowed for trusted internal hosts")
}

func statsigInternalHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast()
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
		strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") {
		return true
	}
	if strings.Contains(host, ".") {
		return false
	}
	// A single label is a container/service name when it looks like one.
	if len(host) < 1 || len(host) > 63 {
		return false
	}
	valid := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '-' || b == '_'
	}
	if !valid(host[0]) || !valid(host[len(host)-1]) {
		return false
	}
	for i := 1; i < len(host)-1; i++ {
		if !valid(host[i]) {
			return false
		}
	}
	return true
}

// validateStatsigConfig checks the configured signer URL. nil (built-in
// signer) and "" (signing disabled) are both valid.
func validateStatsigConfig(cfg *Config) error {
	if cfg == nil || cfg.GrokStatsigSignerURL == nil {
		return nil
	}
	value := strings.TrimSpace(*cfg.GrokStatsigSignerURL)
	if value == "" {
		return nil
	}
	if err := ValidateStatsigSignerURL(value); err != nil {
		return fmt.Errorf("grok_statsig_signer_url: %w", err)
	}
	return nil
}

// ValidateSavedConfig checks fields the admin plane must refuse on save rather
// than discover at request time.
func ValidateSavedConfig(cfg *Config) error {
	return validateStatsigConfig(cfg)
}
