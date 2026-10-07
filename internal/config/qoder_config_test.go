package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"orchids-api/internal/testutil"
)

func TestRetiredQoderProfileIsIgnored(t *testing.T) {
	for _, tc := range []struct{ extension, body string }{
		{"json", `{"admin_pass":"test","qoder_protocol_profile":"skill-cli","qoder_client_id":"override"}`},
		{"yaml", "admin_pass: test\nqoder_protocol_profile: skill-cli\nqoder_client_id: override\n"},
	} {
		t.Run(tc.extension, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config."+tc.extension)
			testutil.NoError(t, os.WriteFile(path, []byte(tc.body), 0600))
			cfg, _, err := Load(path)
			testutil.NoError(t, err)
			testutil.Equal(t, cfg.QoderClientID, "override")
			raw, err := json.Marshal(cfg)
			testutil.NoError(t, err)
			testutil.MustNotContain(t, string(raw), "qoder_protocol_profile")
		})
	}
}
