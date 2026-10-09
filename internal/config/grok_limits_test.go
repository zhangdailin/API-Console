package config

import (
	"orchids-api/internal/testutil"
	"testing"
	"time"

	"encoding/json"
)

func TestGrokBuildLimitsSurviveConfigRoundTrip(t *testing.T) {
	var cfg Config
	testutil.NoError(t, json.Unmarshal([]byte(`{"max_retries":2,"retry_delay":50,"account_switch_count":4,"request_timeout":1800,"concurrency_timeout":2400,"retry_429_interval":90,"grok_build_rps":10,"grok_build_timeout_seconds":1800,"grok_stream_idle_seconds":300}`), &cfg))
	ApplyHardcoded(&cfg)
	raw, err := json.Marshal(cfg)
	testutil.NoError(t, err)
	var restored Config
	testutil.NoError(t, json.Unmarshal(raw, &restored))
	ApplyHardcoded(&restored)
	testutil.False(t, restored.MaxRetries != 2 || restored.RetryDelay != 50 || restored.AccountSwitchCount != 4 || restored.RequestTimeout != 1800 || restored.ConcurrencyTimeout != 2400 || restored.Retry429Interval != 90, "runtime settings were overwritten")
	testutil.False(t, restored.GrokRequestsPerSecond() != 10 || restored.GrokRequestTimeout() != 1800*time.Second, "Build limits lost")
	testutil.Equal(t, restored.GrokStreamIdleTimeoutFor(), 300*time.Second)
}

func TestGrokBuildLimitsDefaultsAndBounds(t *testing.T) {
	var cfg *Config
	testutil.False(t, cfg.GrokRequestsPerSecond() != 0 || cfg.GrokRequestTimeout() != 600*time.Second, "unexpected defaults")
	cfg = &Config{RequestTimeout: 999999, GrokBuildTimeout: 999999, GrokBuildRPS: 999999, GrokStreamIdleSeconds: 999999}
	ApplyHardcoded(cfg)
	testutil.False(t, cfg.RequestTimeout != 86400 || cfg.GrokRequestTimeout() != 24*time.Hour || cfg.GrokStreamIdleTimeoutFor() != 10*time.Minute || cfg.GrokRequestsPerSecond() != 1000, "invalid bounds")
}

func TestGrokBuildIdleDefaultsOverridesAndLegacyFallback(t *testing.T) {
	var cfg *Config
	testutil.Equal(t, cfg.GrokStreamIdleTimeoutFor(), 2*time.Minute)
	cfg = &Config{GrokStreamIdleSeconds: 45, GrokBuildStreamIdleSeconds: 9999}
	testutil.Equal(t, cfg.GrokStreamIdleTimeoutFor(), 10*time.Minute)
	cfg.GrokBuildStreamIdleSeconds = 1
	testutil.Equal(t, cfg.GrokStreamIdleTimeoutFor(), 30*time.Second)
}
