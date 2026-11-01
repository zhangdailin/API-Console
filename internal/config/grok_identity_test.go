package config

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/testutil"
)

func TestGrokIdentityMigratesPersistedLegacyValues(t *testing.T) {
	var cfg Config
	testutil.NoError(t, json.Unmarshal([]byte(`{"admin_pass":"test-password","grok_cli_client_version":"1.0.4","grok_cli_user_agent":"grok-pager/0.2.93 grok-shell/1.0.12 (linux; aarch64)"}`), &cfg))
	// Accessors must also be safe before defaults, without mutating published configs.
	testutil.Equal(t, cfg.GrokCLIClientVersionOrDefault(), "1.0.60")
	testutil.Equal(t, cfg.GrokCLIUserAgentOrDefault(), "grok-pager/1.0.60 grok-shell/1.0.60 (linux; aarch64)")
	testutil.Equal(t, cfg.GrokCLIClientVersion, "1.0.4")
	ApplyDefaults(&cfg)
	testutil.Equal(t, cfg.GrokCLIClientVersion, "1.0.60")
	raw, err := json.Marshal(cfg)
	testutil.NoError(t, err)
	testutil.MustNotContain(t, string(raw), "0.2.93")
	testutil.MustNotContain(t, string(raw), "1.0.12")
}

func TestGrokIdentityPreservesFutureAndCustomValues(t *testing.T) {
	for _, tc := range []struct{ version, ua, wantVersion, wantUA string }{
		{"1.0.100", "", "1.0.100", "grok-shell/1.0.100 (linux; x86_64)"},
		{"2.0.0", "grok-shell/2.0.0 (darwin; arm64)", "2.0.0", "grok-shell/2.0.0 (darwin; arm64)"},
		{"custom-version", "custom-client/0.1", "custom-version", "custom-client/0.1"},
		{"1.0.60", "custom-grok-shell/0.1.0", "1.0.60", "custom-grok-shell/0.1.0"},
		{"1.0.60-rc.1", "grok-shell/1.0.60-rc.1", "1.0.60", "grok-shell/1.0.60"},
	} {
		cfg := Config{GrokCLIClientVersion: tc.version, GrokCLIUserAgent: tc.ua}
		ApplyHardcoded(&cfg)
		testutil.Equal(t, cfg.GrokCLIClientVersionOrDefault(), tc.wantVersion)
		testutil.Equal(t, cfg.GrokCLIUserAgentOrDefault(), tc.wantUA)
	}
	var cfg *Config
	testutil.Equal(t, cfg.GrokCLIClientVersionOrDefault(), "1.0.60")
	testutil.Equal(t, cfg.GrokCLIUserAgentOrDefault(), "grok-shell/1.0.60 (linux; x86_64)")
}
