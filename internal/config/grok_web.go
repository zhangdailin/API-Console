package config

import (
	"strings"
	"time"
)

// Grok Web (grok.com SSO) defaults.
const (
	DefaultGrokWebBaseURL   = "https://grok.com"
	DefaultGrokWebUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
)

func cloneOptional[T any](value *T) *T {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

// GrokWebBaseURLOrDefault returns the grok.com origin used by Web SSO accounts.
func (c *Config) GrokWebBaseURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokWebBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.GrokWebBaseURL), "/")
	}
	return DefaultGrokWebBaseURL
}

// GrokWebUserAgentOrDefault returns the browser user agent sent to grok.com.
func (c *Config) GrokWebUserAgentOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokWebUserAgent) != "" {
		return strings.TrimSpace(c.GrokWebUserAgent)
	}
	return DefaultGrokWebUserAgent
}

// GrokChatTemporary reports whether Web conversations are created as
// temporary (not kept in the account's grok.com history). Default on.
func (c *Config) GrokChatTemporary() bool {
	return c == nil || c.GrokTemporary == nil || *c.GrokTemporary
}

func (c *Config) GrokChatDisableMemory(defaultValue bool) bool {
	if c == nil || c.GrokDisableMemory == nil {
		return defaultValue
	}
	return *c.GrokDisableMemory
}

func (c *Config) GrokChatCustomInstruction() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.GrokCustomInstruction)
}

func (c *Config) GrokClearanceRefreshIntervalOrDefault() int {
	if c != nil && c.GrokClearanceRefreshInterv > 0 {
		return c.GrokClearanceRefreshInterv
	}
	return 600
}

func (c *Config) GrokClearanceModeOrDefault() string {
	if c != nil {
		switch mode := strings.ToLower(strings.TrimSpace(c.GrokClearanceMode)); mode {
		case "flaresolverr", "on_demand":
			return mode
		}
	}
	return "manual"
}

// GrokClearanceTimeoutOrDefault bounds one FlareSolverr solve (10s–300s).
func (c *Config) GrokClearanceTimeoutOrDefault() time.Duration {
	seconds := 60
	if c != nil && c.GrokClearanceTimeout > 0 {
		seconds = min(max(c.GrokClearanceTimeout, 10), 300)
	}
	return time.Duration(seconds) * time.Second
}

func (c *Config) PublicImagineNSFW() bool {
	return c == nil || c.GrokImageNSFW == nil || *c.GrokImageNSFW
}

func (c *Config) PublicImagineFinalMinBytes() int {
	if c == nil || c.GrokImageFinalMinBytes <= 0 {
		return 100000
	}
	return c.GrokImageFinalMinBytes
}

func (c *Config) PublicImagineMediumMinBytes() int {
	if c == nil || c.GrokImageMediumMinBytes <= 0 {
		return 30000
	}
	return c.GrokImageMediumMinBytes
}

// GrokWebRequestsPerSecond is the optional per-account pacing for Web SSO.
func (c *Config) GrokWebRequestsPerSecond() float64 {
	if c == nil || c.GrokWebRPS <= 0 {
		return 0
	}
	return min(max(c.GrokWebRPS, 0.01), 1000)
}

// GrokWebRequestTimeout bounds one Web upstream request.
func (c *Config) GrokWebRequestTimeout() time.Duration {
	fallback := int(InferenceRequestTimeout.Seconds())
	value := 0
	if c != nil {
		value = c.GrokWebTimeout
	}
	return time.Duration(boundedDefault(value, fallback, 86400)) * time.Second
}

// GrokWebStreamIdleTimeout is the Web stream idle watchdog.
func (c *Config) GrokWebStreamIdleTimeout() time.Duration {
	value := 0
	if c != nil {
		value = c.GrokWebStreamIdleSeconds
	}
	seconds := max(boundedDefault(value, 90, 600), 30)
	return time.Duration(seconds) * time.Second
}

// GrokWebTransport returns the config the Web plane builds its HTTP clients
// from: when grok_web_proxy_url is set it replaces the general proxy settings
// for grok.com traffic only; otherwise the config is returned unchanged.
func (c *Config) GrokWebTransport() *Config {
	if c == nil || strings.TrimSpace(c.GrokWebProxyURL) == "" {
		return c
	}
	clone := c.Clone()
	clone.ProxyURL = strings.TrimSpace(c.GrokWebProxyURL)
	clone.ProxyHTTP, clone.ProxyHTTPS, clone.ProxyUser, clone.ProxyPass = "", "", "", ""
	clone.ProxyBypass = nil
	return clone
}
