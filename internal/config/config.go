package config

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"encoding/json"
)

type Config struct {
	// ── Configurable fields (read from config.json / Redis) ──
	Port                        string         `json:"port"`
	DebugEnabled                bool           `json:"debug_enabled"`
	VerboseDiagnostics          bool           `json:"verbose_diagnostics,omitempty"`
	AdminUser                   string         `json:"admin_user"`
	AdminPass                   string         `json:"admin_pass"`
	AdminPath                   string         `json:"admin_path"`
	AdminToken                  string         `json:"admin_token"`
	CredentialKeyFile           string         `json:"credential_encryption_key_file,omitempty"`
	ResponseStoreTTL            int            `json:"response_store_ttl_hours,omitempty"`
	TrustedProxies              []string       `json:"trusted_proxies,omitempty"`
	RedisAddr                   string         `json:"redis_addr"`
	RedisPassword               string         `json:"redis_password"`
	RedisDB                     int            `json:"redis_db"`
	RedisPoolSize               int            `json:"redis_pool_size,omitempty"`
	RedisPrefix                 string         `json:"redis_prefix"`
	DeploymentInstance          string         `json:"deployment_instance_id,omitempty"`
	MediaDir                    string         `json:"media_dir,omitempty"`
	CacheStrategy               string         `json:"cache_strategy"`
	UpstreamMaxConnsPerHost     int            `json:"upstream_max_conns_per_host,omitempty"`
	UpstreamMaxIdleConnsPerHost int            `json:"upstream_max_idle_conns_per_host,omitempty"`
	ClineHTTP2Enabled           bool           `json:"cline_http2_enabled,omitempty"`
	WorkBuddyHTTP2Enabled       bool           `json:"workbuddy_http2_enabled,omitempty"`
	DiagnosticsSampleEvery      int            `json:"diagnostics_sample_every,omitempty"`
	DiagnosticsMaxConcurrent    int            `json:"diagnostics_max_concurrent,omitempty"`
	ProviderConcurrencyLimits   map[string]int `json:"provider_concurrency_limits,omitempty"`
	StreamFlushIntervalMs       int            `json:"stream_flush_interval_ms,omitempty"`

	// ── Hardcoded fields (set unconditionally by ApplyHardcoded) ──
	DebugLogSSE      bool `json:"-"`
	SuppressThinking bool `json:"-"`
	// AnonymousAllowIPs names the sources that may call the inference routes
	// without a managed key. Empty (the default) requires a key from everyone, as
	// the default. An operator that cannot update a client yet lists its address
	// here, and every other caller still needs a key.
	AnonymousAllowIPs []string `json:"anonymous_allow_ips,omitempty"`

	// ── WorkBuddy international backend (www.workbuddy.ai) ──
	// Overridable for self-hosted regional deployments and for tests that need a
	// stubbed upstream. Empty means the production international host.
	WorkBuddyDefaultMaxTokens int    `json:"workbuddy_default_max_tokens,omitempty"`
	WorkBuddyBaseURL          string `json:"workbuddy_base_url,omitempty"`

	// ── Qoder (qoder.com CLI device authorization) ──
	// The Qoder CLI talks to three hosts. They are configurable so a deployment
	// can point at a regional endpoint (for example the CN gateway) and so tests
	// can stub the upstream.
	//
	//   QoderOAuthBaseURL   browser authorization page   (default https://qoder.com)
	//   QoderOpenAPIBaseURL device token + profile API   (default https://openapi.qoder.sh)
	//   QoderInferenceURL   chat completion endpoint     (default https://api2.qoder.sh)
	QoderOAuthBaseURL   string `json:"qoder_oauth_base_url,omitempty"`
	QoderOpenAPIBaseURL string `json:"qoder_openapi_base_url,omitempty"`
	QoderInferenceURL   string `json:"qoder_inference_base_url,omitempty"`
	// QoderClientID is the public OAuth client id of the Qoder CLI. It is not a
	// secret, and it is configurable so a future CLI build can be followed
	// without a code change.
	QoderClientID string `json:"qoder_client_id,omitempty"`
	// QoderClientVersion is sent as the user agent / Cosy-Version family value
	// and appended to the login URL.
	QoderClientVersion string `json:"qoder_client_version,omitempty"`
	// QoderHTTP2Enabled makes the Qoder chat transport use HTTP/2, matching the
	// official client's offer on the wire. It is off by default because HTTP/2
	// multiplexes every chat request to the host over one connection, so a
	// single reset there fails every in-flight request at once, and Go queues at
	// the peer's stream limit instead of opening another connection. It was also
	// measured *not* to be the cause of the queue refusals: HTTP/1.1, an
	// explicit HTTP/2 transport and the default client all answered in the same
	// 1.5-2.0s window, and the refusals appeared on all three.
	QoderHTTP2Enabled bool `json:"qoder_http2_enabled,omitempty"`

	// ── Cline (api.cline.bot) OAuth channel ──
	//
	// The channel talks to two hosts: the Cline API (control plane, inference and
	// the model feed) and WorkOS, which performs the device authorization. They
	// are configurable so a deployment can point at a regional endpoint and so
	// tests can stub the upstream. Empty means the production hosts.
	//
	//   ClineAPIBaseURL      Cline API                    (default https://api.cline.bot/api/v1)
	//   ClineWorkOSClientID  public WorkOS client id      (default the Cline CLI's)
	//   ClineWorkOSAuthorizeURL / ClineWorkOSTokenURL
	//                        the device authorization pair (default api.workos.com)
	ClineAPIBaseURL         string `json:"cline_api_base_url,omitempty"`
	ClineWorkOSClientID     string `json:"cline_workos_client_id,omitempty"`
	ClineWorkOSAuthorizeURL string `json:"cline_workos_authorize_url,omitempty"`
	ClineWorkOSTokenURL     string `json:"cline_workos_token_url,omitempty"`

	// ── Grok Build CLI (cli-chat-proxy.grok.com) OAuth upstream ──
	// These fields are configurable via config.json / Redis and are deliberately
	// NOT written into ApplyHardcoded, so they survive a persistConfig round trip.
	GrokCLIBaseURL          string  `json:"grok_cli_base_url,omitempty"`
	GrokCLIUserAgent        string  `json:"grok_cli_user_agent,omitempty"`
	GrokCLIClientVersion    string  `json:"grok_cli_client_version,omitempty"`
	GrokCLIClientIdentifier string  `json:"grok_cli_client_identifier,omitempty"`
	GrokCLIOAuthClientID    string  `json:"grok_cli_oauth_client_id,omitempty"`
	GrokCLIOAuthDeviceURL   string  `json:"grok_cli_oauth_device_url,omitempty"`
	GrokCLIOAuthTokenURL    string  `json:"grok_cli_oauth_token_url,omitempty"`
	GrokBuildRPS            float64 `json:"grok_build_rps,omitempty"`
	GrokBuildTimeout        int     `json:"grok_build_timeout_seconds,omitempty"`
	// GrokStreamIdleSeconds is the legacy all-channel fallback. The channel
	// Build-specific field below takes precedence when set.
	GrokStreamIdleSeconds      int `json:"grok_stream_idle_seconds,omitempty"`
	GrokBuildStreamIdleSeconds int `json:"grok_build_stream_idle_seconds,omitempty"`

	// ── Grok Build egress proxy pool ──
	GrokEgressEnabled bool               `json:"grok_egress_enabled,omitempty"`
	GrokEgressNodes   []EgressNodeConfig `json:"grok_egress_nodes,omitempty"`

	// ── Upstream fidelity (defaults preserve client content verbatim) ──
	// A relay gateway forwards client messages without rewriting content.
	// This field is NOT written into ApplyHardcoded, so it survives a
	// persistConfig round trip.
	//
	// Two inert trimming knobs used to live here. They never trimmed anything —
	// the only reader was the account client-cache key — so a deployment that
	// lowered them to save context got no saving and no warning. They are gone
	// rather than documented, because an inert knob is a trap.
	Stream             *bool `json:"-"`
	MaxRetries         int   `json:"max_retries,omitempty"`
	RetryDelay         int   `json:"retry_delay,omitempty"`
	AccountSwitchCount int   `json:"account_switch_count,omitempty"`
	// SharedRefusalWaitBudgetMs bounds how long one request may wait on a
	// resource every account shares — Qoder's model queue gate being the one
	// that matters in practice. The upstream's own retry hint is still honoured
	// per attempt; this only stops a closed gate from holding a caller past the
	// point where the edition in front of it gives up.
	//
	// Zero selects the built-in default, 60s: two of Qoder's 30s windows, which
	// costs about 72s of wall clock and fits inside a 100s edge origin timeout.
	// That edge timeout, not the caller's patience, is the binding deadline —
	// publishing through Cloudflare with a larger budget produced 520s at the
	// edge for requests the gateway would have answered at ~106s. The value is
	// clamped to a day so a bad setting cannot pin a request forever, but
	// anything above 60s is warned about at startup for that reason.
	SharedRefusalWaitBudgetMs int `json:"shared_refusal_wait_budget_ms,omitempty"`
	// QoderQueueRetryIntervalMs overrides the queue retry interval. Zero keeps
	// the upstream hint; a positive value schedules retries at this interval.
	SharedStreamIdleTimeoutSeconds int `json:"shared_stream_idle_timeout_seconds,omitempty"`
	FirstTokenTimeoutSeconds       int `json:"first_token_timeout_seconds,omitempty"`
	QoderQueueWaitBudgetMs         int `json:"qoder_queue_wait_budget_ms,omitempty"`
	QoderQueueRetryIntervalMs      int `json:"qoder_queue_retry_interval_ms,omitempty"`
	// Quality-hold policy. The gateway withholds a degraded reasoning turn
	// instead of streaming it, then retries it on another account. Holding is on
	// by default and fails open once the retry budget is spent.
	QualityHoldEnabled     *bool    `json:"quality_hold_enabled,omitempty"`
	QualityHoldMaxAttempts int      `json:"quality_hold_max_attempts,omitempty"`
	QualityHoldTimeoutMs   int      `json:"quality_hold_timeout_ms,omitempty"`
	QualityHoldOnExhausted string   `json:"quality_hold_on_exhausted,omitempty"`
	RequestTimeout         int      `json:"request_timeout,omitempty"`
	Retry429Interval       int      `json:"retry_429_interval,omitempty"`
	TokenRefreshInterval   int      `json:"-"`
	AutoRefreshToken       bool     `json:"-"`
	LoadBalancerCacheTTL   int      `json:"-"`
	ConcurrencyLimit       int      `json:"concurrency_limit,omitempty"`
	ConcurrencyTimeout     int      `json:"concurrency_timeout,omitempty"`
	ProxyURL               string   `json:"proxy_url"`
	ProxyHTTP              string   `json:"proxy_http"`
	ProxyHTTPS             string   `json:"proxy_https"`
	ProxyUser              string   `json:"proxy_user"`
	ProxyPass              string   `json:"proxy_pass"`
	ProxyBypass            []string `json:"proxy_bypass"`
}

// EgressNodeConfig describes one egress exit node for the Grok proxy pool.
// Defined in the config package (not grok/egress) to avoid an import cycle.
type EgressNodeConfig struct {
	Name    string `json:"name"`
	URL     string `json:"url"`    // proxy address http/socks5; empty = direct
	Weight  int    `json:"weight"` // weight for weighted round-robin; <=0 = 1
	Scope   string `json:"scope"`  // "cli"|"all"
	Proxied bool   `json:"proxied"`
}

// Clone returns a deep copy suitable for publishing as an immutable runtime
// snapshot. Config contains pointer and slice fields, so a plain struct copy
// would still let a later request mutate data observed by concurrent readers.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}

	clone := *c
	clone.Stream = cloneBool(c.Stream)
	clone.TrustedProxies = append([]string(nil), c.TrustedProxies...)
	clone.GrokEgressNodes = append([]EgressNodeConfig(nil), c.GrokEgressNodes...)
	clone.ProxyBypass = append([]string(nil), c.ProxyBypass...)
	if c.ProviderConcurrencyLimits != nil {
		clone.ProviderConcurrencyLimits = make(map[string]int, len(c.ProviderConcurrencyLimits))
		for key, value := range c.ProviderConcurrencyLimits {
			clone.ProviderConcurrencyLimits[key] = value
		}
	}
	return &clone
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func Load(path string) (*Config, string, error) {
	resolvedPath, err := resolveConfigPath(path)
	if err != nil {
		return nil, "", err
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read config: %w", err)
	}

	cfg := Config{}
	ext := strings.ToLower(filepath.Ext(resolvedPath))
	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse config json: %w", err)
		}
	case ".yaml", ".yml":
		m, err := parseYAMLFlat(data)
		if err != nil {
			return nil, "", err
		}
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, "", fmt.Errorf("failed to normalize yaml: %w", err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, "", fmt.Errorf("failed to parse config yaml: %w", err)
		}
	default:
		return nil, "", fmt.Errorf("unsupported config extension: %s", ext)
	}

	ApplyDefaults(&cfg)
	if err := ValidateProxyConfig(&cfg); err != nil {
		return nil, "", fmt.Errorf("invalid proxy config: %w", err)
	}
	return &cfg, resolvedPath, nil
}

func resolveConfigPath(path string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return path, nil
	}

	candidates := []string{"config.json", "config.yaml", "config.yml"}
	for _, name := range candidates {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}

	return "", errors.New("config.json/config.yaml/config.yml not found")
}

func ApplyDefaults(cfg *Config) {
	if cfg.Port == "" {
		cfg.Port = "3002"
	}
	if cfg.AdminUser == "" {
		cfg.AdminUser = "admin"
	}
	if cfg.AdminPass == "" {
		generated, err := generateRandomPassword(16)
		if err != nil {
			slog.Error("无法生成随机密码", "error", err)
			os.Exit(1)
		}
		cfg.AdminPass = generated
		slog.Warn("未设置 admin_pass，已自动生成随机密码，请在配置文件中设置 admin_pass",
			"generated_password", generated)
	}
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}
	if cfg.RedisPrefix == "" {
		cfg.RedisPrefix = "orchids:"
	}
	if strings.TrimSpace(cfg.MediaDir) == "" {
		cfg.MediaDir = filepath.Join("data", "tmp")
	}
	if strings.TrimSpace(cfg.CredentialKeyFile) == "" {
		cfg.CredentialKeyFile = filepath.Join("data", "credential.key")
	}
	if cfg.ResponseStoreTTL <= 0 {
		cfg.ResponseStoreTTL = 30 * 24
	}
	if strings.TrimSpace(cfg.CacheStrategy) == "" {
		cfg.CacheStrategy = "mix"
	}
	// Fidelity default: preserve client content verbatim unless explicitly
	// configured otherwise ("auto"/"strip" re-enable cc_entrypoint handling).
	// Always apply hardcoded values
	ApplyHardcoded(cfg)
}

// ApplyHardcoded sets fixed fields and supplies bounded defaults for runtime
// retry/deadline settings. Configured runtime values survive file/Redis/API
// round trips; protocol constants remain non-configurable.
func ApplyHardcoded(cfg *Config) {
	vTrue := true
	cfg.Stream = &vTrue
	cfg.MaxRetries = boundedDefault(cfg.MaxRetries, 3, 20)
	cfg.WorkBuddyDefaultMaxTokens = cfg.WorkBuddyOutputBudget()
	if cfg.FirstTokenTimeoutSeconds < 0 {
		cfg.FirstTokenTimeoutSeconds = -1
	} else {
		cfg.FirstTokenTimeoutSeconds = boundedDefault(cfg.FirstTokenTimeoutSeconds, 60, 86400)
	}
	cfg.QoderQueueWaitBudgetMs = max(-1, min(cfg.QoderQueueWaitBudgetMs, 86400000))
	cfg.SharedStreamIdleTimeoutSeconds = max(30, boundedDefault(cfg.SharedStreamIdleTimeoutSeconds, 300, 600))
	cfg.RetryDelay = boundedDefault(cfg.RetryDelay, 1000, 60000)
	// The shared-refusal wait budget is a ceiling, not a minimum: zero asks for
	// the built-in 90s, and an operator may raise it up to a day. It is not
	// lowered to the retry delay, because the two answer different questions —
	// retry_delay is how fast a fresh attempt starts, this is how long one
	// request tolerates an upstream gate.
	cfg.SharedRefusalWaitBudgetMs = boundedDefault(cfg.SharedRefusalWaitBudgetMs, 60000, 86400000)
	// How many accounts one request may rotate through before it gives up.
	// The reference implementation allows far more; a ceiling of twenty made a
	// bad pool fail visibly ("retries exhausted") while equivalent accounts were
	// still available. Account-level cooldowns bound the retries, so a larger
	// budget does not turn into a retry storm.
	cfg.AccountSwitchCount = boundedDefault(cfg.AccountSwitchCount, 20, 100)
	// A long reasoning or tool-using turn is legitimate: the reference
	// implementation allows two hours, and a ten minute ceiling cut such a turn
	// short while the per-channel stream-idle watchdog already bounds a stalled
	// one. The bounds still let an operator lower it.
	cfg.RequestTimeout = boundedDefault(cfg.RequestTimeout, 7200, 86400)
	cfg.Retry429Interval = boundedDefault(cfg.Retry429Interval, 60, 3600)
	cfg.TokenRefreshInterval = 1
	cfg.AutoRefreshToken = true
	cfg.LoadBalancerCacheTTL = 5
	if cfg.ConcurrencyLimit <= 0 {
		cfg.ConcurrencyLimit = 100
	}
	if cfg.ConcurrencyLimit > 1000000 {
		cfg.ConcurrencyLimit = 1000000
	}
	cfg.ConcurrencyTimeout = boundedDefault(cfg.ConcurrencyTimeout, cfg.RequestTimeout, 86400)
	cfg.DebugLogSSE = cfg.DebugEnabled
}

func (c *Config) VerboseDiagnosticsEnabled() bool {
	return c != nil && c.DebugEnabled && c.VerboseDiagnostics
}

func (c *Config) ChatDefaultStream() bool { return c == nil || c.Stream == nil || *c.Stream }

// GrokCLIBaseURLOrDefault returns the Build CLI proxy base URL, defaulting to
// the official gateway.
func (c *Config) GrokCLIBaseURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(c.GrokCLIBaseURL), "/")
	}
	return "https://cli-chat-proxy.grok.com/v1"
}

// GrokCLIOAuthClientIDOrDefault returns the xAI OAuth client ID used for Build
// token refresh, defaulting to the official CLI client.
func (c *Config) GrokCLIOAuthClientIDOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIOAuthClientID) != "" {
		return strings.TrimSpace(c.GrokCLIOAuthClientID)
	}
	return "b1a00492-073a-47ea-816f-4c329264a828"
}

// GrokCLIOAuthDeviceURLOrDefault returns the xAI OAuth device-authorization
// endpoint used by the official Grok Build CLI.
func (c *Config) GrokCLIOAuthDeviceURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIOAuthDeviceURL) != "" {
		return strings.TrimSpace(c.GrokCLIOAuthDeviceURL)
	}
	return "https://auth.x.ai/oauth2/device/code"
}

// GrokCLIOAuthTokenURLOrDefault returns the xAI OAuth token endpoint.
func (c *Config) GrokCLIOAuthTokenURLOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIOAuthTokenURL) != "" {
		return strings.TrimSpace(c.GrokCLIOAuthTokenURL)
	}
	return "https://auth.x.ai/oauth2/token"
}

// GrokCLIUserAgentOrDefault returns the CLI user agent stamped on Build
// requests. A fixed CLI identity (not the browser UA) is required upstream.
func (c *Config) GrokCLIUserAgentOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIUserAgent) != "" {
		return strings.TrimSpace(c.GrokCLIUserAgent)
	}
	return "grok-shell/1.0.40 (linux; x86_64)"
}

// GrokCLIClientVersionOrDefault returns the x-grok-client-version header value.
func (c *Config) GrokCLIClientVersionOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIClientVersion) != "" {
		return strings.TrimSpace(c.GrokCLIClientVersion)
	}
	return "1.0.40"
}

// GrokCLIClientIdentifierOrDefault returns the x-grok-client-identifier header.
func (c *Config) GrokCLIClientIdentifierOrDefault() string {
	if c != nil && strings.TrimSpace(c.GrokCLIClientIdentifier) != "" {
		return strings.TrimSpace(c.GrokCLIClientIdentifier)
	}
	return "grok-shell"
}

func generateRandomPassword(length int) (string, error) {
	// hex encoding doubles the length, so we only need half the bytes
	byteLen := (length + 1) / 2
	b := make([]byte, byteLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	encoded := hex.EncodeToString(b)
	if len(encoded) > length {
		encoded = encoded[:length]
	}
	return encoded, nil
}

func parseYAMLFlat(data []byte) (map[string]interface{}, error) {
	out := map[string]interface{}{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Only strip inline comments where # is preceded by whitespace,
		// to avoid corrupting values containing # (hex colors, URLs, etc.)
		if idx := strings.Index(line, " #"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		} else if idx := strings.Index(line, "\t#"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid yaml line: %q", line)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, "\"'")

		if key == "" {
			continue
		}
		if value == "" {
			out[key] = ""
			continue
		}
		if value == "true" || value == "false" {
			out[key] = value == "true"
			continue
		}
		if num, err := strconv.Atoi(value); err == nil {
			out[key] = num
			continue
		}
		out[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
