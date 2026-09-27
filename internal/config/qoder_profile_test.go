package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestQoderProtocolProfileConfigRoundTrip(t *testing.T) {
	for _, ext := range []string{"json", "yaml"} {
		t.Run(ext, func(t *testing.T) {
			raw := []byte(`{"qoder_protocol_profile":"skill-cli","qoder_client_id":"override"}`)
			if ext == "yaml" {
				raw = []byte("qoder_protocol_profile: skill-cli\nqoder_client_id: override\n")
			}
			path := filepath.Join(t.TempDir(), "config."+ext)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			cfg, _, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.QoderProtocolProfile != "skill-cli" || cfg.QoderClientID != "override" {
				t.Fatal("profile or override lost loading")
			}
			encoded, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			var round Config
			if err = json.Unmarshal(encoded, &round); err != nil {
				t.Fatal(err)
			}
			if round.QoderProtocolProfile != "skill-cli" {
				t.Fatal("profile lost serializing")
			}
		})
	}
}
