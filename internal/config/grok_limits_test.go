package config

import (
	"encoding/json"
	"orchids-api/internal/testutil"
	"testing"
	"time"
)

func TestGrokPacingSurvivesConfigRoundTrip(t *testing.T) {
	var cfg Config
	testutil.NoError(t, json.Unmarshal([]byte(`{"grok_build_rps":10,"grok_build_timeout_seconds":1,"grok_stream_idle_seconds":1,"grok_build_stream_idle_seconds":1}`), &cfg))
	ApplyHardcoded(&cfg)
	raw, err := json.Marshal(cfg)
	testutil.NoError(t, err)
	var restored Config
	testutil.NoError(t, json.Unmarshal(raw, &restored))
	ApplyHardcoded(&restored)
	testutil.Equal(t, restored.GrokRequestsPerSecond(), float64(10))
	testutil.Equal(t, restored.UpstreamRequestTimeout(), InferenceRequestTimeout)
	testutil.MustNotContainAny(t, string(raw), "grok_build_timeout_seconds", "grok_stream_idle_seconds", "grok_build_stream_idle_seconds")
}
func TestGrokPacingBoundsAndInternalCatalogTimeout(t *testing.T) {
	var cfg *Config
	testutil.Equal(t, cfg.GrokRequestsPerSecond(), float64(0))
	testutil.Equal(t, cfg.UpstreamRequestTimeout(), InferenceRequestTimeout)
	cfg = &Config{GrokBuildRPS: 999999, RequestTimeout: 999999}
	testutil.Equal(t, cfg.GrokRequestsPerSecond(), float64(1000))
	testutil.Equal(t, cfg.UpstreamRequestTimeout(), InferenceRequestTimeout)
	cfg.RequestTimeout = 15
	testutil.Equal(t, cfg.UpstreamRequestTimeout(), 15*time.Second)
}
