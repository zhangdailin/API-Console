package api

import (
	"context"
	"encoding/json"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestPersistConfigMigratesGrokIdentity(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	cfg := a.config.Load().Clone()
	cfg.GrokCLIClientVersion = "1.0.4"
	cfg.GrokCLIUserAgent = "grok-shell/1.0.4 (linux; aarch64)"
	testutil.NoError(t, a.persistConfig(context.Background(), nil, cfg))
	raw, err := s.GetSetting(context.Background(), "config")
	testutil.NoError(t, err)
	var saved config.Config
	testutil.NoError(t, json.Unmarshal([]byte(raw), &saved))
	testutil.Equal(t, saved.GrokCLIClientVersion, "1.0.60")
	testutil.Equal(t, saved.GrokCLIUserAgent, "grok-shell/1.0.60 (linux; aarch64)")
	testutil.Equal(t, a.config.Load().GrokCLIClientVersion, "1.0.60")
}
