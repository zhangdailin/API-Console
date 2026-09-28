package config

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)

	if got := cfg.ChatDefaultStream(); got != true {
		t.Fatalf("ChatDefaultStream()=%v want=true", got)
	}
	if cfg.ResponseStoreTTL != 720 {
		t.Fatalf("ResponseStoreTTL=%d want=720", cfg.ResponseStoreTTL)
	}
	if cfg.DeploymentReplicas != 1 || cfg.DeploymentCluster != "orchids" {
		t.Fatalf("deployment defaults = replicas %d cluster %q", cfg.DeploymentReplicas, cfg.DeploymentCluster)
	}
	if cfg.MediaDir != "data"+string(filepath.Separator)+"tmp" {
		t.Fatalf("MediaDir=%q", cfg.MediaDir)
	}
	if got := cfg.GrokCLIClientVersionOrDefault(); got != "1.0.40" {
		t.Fatalf("GrokCLIClientVersionOrDefault()=%q", got)
	}
	if got := cfg.GrokCLIUserAgentOrDefault(); got != "grok-shell/1.0.40 (linux; x86_64)" {
		t.Fatalf("GrokCLIUserAgentOrDefault()=%q", got)
	}
}

// A conversation binding must outlive an ordinary working session. Thirty
// minutes detached the upstream conversation between turns, which forced the
// next turn to replay the whole transcript.
func TestConfigDefaultsKeepConversationBindingsAlive(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.SessionTTLMinutes < 60 {
		t.Fatalf("SessionTTLMinutes=%d, want at least an hour", cfg.SessionTTLMinutes)
	}
}

// An operator's explicit values survive, and an absurd one is still bounded.
func TestConfigKeepsExplicitContextSettingsWithinBounds(t *testing.T) {
	cfg := Config{SessionTTLMinutes: 90}
	ApplyDefaults(&cfg)
	if cfg.SessionTTLMinutes != 90 {
		t.Fatalf("SessionTTLMinutes=%d, want the configured 90", cfg.SessionTTLMinutes)
	}

	over := Config{SessionTTLMinutes: 1 << 30}
	ApplyDefaults(&over)
	if over.SessionTTLMinutes > 30*24*60 {
		t.Fatalf("SessionTTLMinutes=%d is unbounded", over.SessionTTLMinutes)
	}
}

func TestCloneDeepCopiesReferenceFields(t *testing.T) {
	original := &Config{
		TrustedProxies:  []string{"10.0.0.1"},
		GrokEgressNodes: []EgressNodeConfig{{Name: "primary", URL: "http://proxy"}},
		ProxyBypass:     []string{"localhost"},
	}

	clone := original.Clone()
	clone.TrustedProxies[0] = "10.0.0.2"
	clone.GrokEgressNodes[0].Name = "changed"
	clone.ProxyBypass[0] = "example.com"

	if original.TrustedProxies[0] != "10.0.0.1" || original.GrokEgressNodes[0].Name != "primary" ||
		original.ProxyBypass[0] != "localhost" {
		t.Fatalf("Clone shares mutable fields with original: %#v", original)
	}
}

func TestApplyDefaultsGeneratesRandomPassword(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)

	if cfg.AdminPass == "" {
		t.Fatal("AdminPass should not be empty after ApplyDefaults")
	}
	if cfg.AdminPass == "admin123" {
		t.Fatal("AdminPass should not be the old default 'admin123'")
	}
	if len(cfg.AdminPass) < 16 {
		t.Fatalf("AdminPass too short: got %d chars, want at least 16", len(cfg.AdminPass))
	}

	// Verify each call generates a different password.
	var cfg2 Config
	ApplyDefaults(&cfg2)
	if cfg.AdminPass == cfg2.AdminPass {
		t.Fatal("Two calls to ApplyDefaults should generate different passwords")
	}
}

func TestApplyHardcodedOverridesValues(t *testing.T) {
	cfg := Config{
		MaxRetries:     999,
		RequestTimeout: 999,
	}
	ApplyHardcoded(&cfg)

	if cfg.MaxRetries != 20 {
		t.Fatalf("MaxRetries=%d want bounded maximum 20", cfg.MaxRetries)
	}
	if cfg.RequestTimeout != 999 {
		t.Fatalf("RequestTimeout=%d want configured 999", cfg.RequestTimeout)
	}
	if cfg.ConcurrencyTimeout != cfg.RequestTimeout {
		t.Fatalf("ConcurrencyTimeout=%d want RequestTimeout=%d", cfg.ConcurrencyTimeout, cfg.RequestTimeout)
	}
}

func TestApplyDefaultsPreservesConfigurableFields(t *testing.T) {
	cfg := Config{
		Port:               "8080",
		AdminUser:          "myuser",
		AdminPass:          "mypass",
		AdminPath:          "/myadmin",
		RedisAddr:          "redis:6380",
		DeploymentReplicas: 3,
		DeploymentInstance: "replica-a",
		DeploymentCluster:  "cluster-a",
		SharedMedia:        true,
		MediaDir:           "/srv/orchids-media",
	}
	ApplyDefaults(&cfg)

	if cfg.Port != "8080" {
		t.Fatalf("Port=%q want=8080", cfg.Port)
	}
	if cfg.AdminUser != "myuser" {
		t.Fatalf("AdminUser=%q want=myuser", cfg.AdminUser)
	}
	if cfg.AdminPass != "mypass" {
		t.Fatalf("AdminPass=%q want=mypass", cfg.AdminPass)
	}
	if cfg.AdminPath != "/myadmin" {
		t.Fatalf("AdminPath=%q want=/myadmin", cfg.AdminPath)
	}
	if cfg.RedisAddr != "redis:6380" {
		t.Fatalf("RedisAddr=%q want=redis:6380", cfg.RedisAddr)
	}
	if cfg.DeploymentReplicas != 3 || cfg.DeploymentInstance != "replica-a" || cfg.DeploymentCluster != "cluster-a" || !cfg.SharedMedia || cfg.MediaDir != "/srv/orchids-media" {
		t.Fatalf("deployment fields were not preserved: %+v", cfg)
	}
}

// Legacy inference_auth_enabled values must not disable managed-key checks.
// Unknown JSON config keys are ignored, including this removed switch.
func TestLegacyInferenceAuthOptOutIsIgnored(t *testing.T) {
	var cfg Config
	if err := json.Unmarshal([]byte(`{"inference_auth_enabled":false}`), &cfg); err != nil {
		t.Fatal(err)
	}
	ApplyDefaults(&cfg)
	if cfg.AnonymousAllowIPs != nil {
		t.Fatal("legacy opt-out must not introduce an anonymous allowlist")
	}
}
