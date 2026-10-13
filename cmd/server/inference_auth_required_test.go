package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func createRouteTestKey(t *testing.T, s *store.Store) string {
	t.Helper()
	token := "test-managed-inference-key"
	digest := sha256.Sum256([]byte(token))
	testutil.NoError(t, s.CreateApiKey(context.Background(), &store.ApiKey{Name: "route-check", KeyHash: hex.EncodeToString(digest[:]), Enabled: true}))
	return token
}

func TestInferenceRoutesRequireKeyRegardlessOfSourceAndLegacyConfig(t *testing.T) {
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admin-token", AdminPath: "/admin", TrustedProxies: []string{"127.0.0.1"}}
	testutil.NoError(t, json.Unmarshal([]byte(`{"inference_auth_enabled":false,"anonymous_allow_ips":["0.0.0.0/0","::/0"],"first_token_timeout_seconds":-1}`), cfg))
	mux, s, _ := newRouteMux(t, "required-auth:", cfg)
	token := createRouteTestKey(t, s)
	for _, prefix := range channel.AllPrefixes() {
		for _, suffix := range []string{"/models", "/messages", "/messages/count_tokens", "/chat/completions", "/responses", "/responses/compact", "/responses/resp_test", "/responses/resp_test/cancel", "/responses/resp_test/input_items"} {
			for _, source := range []string{"127.0.0.1:1234", "192.0.2.1:1234", "[::1]:1234"} {
				method := http.MethodPost
				if suffix == "/models" || suffix == "/responses/resp_test" || suffix == "/responses/resp_test/input_items" {
					method = http.MethodGet
				}
				req := httptest.NewRequest(method, prefix+suffix, strings.NewReader(`{}`))
				req.RemoteAddr = source
				req.Header.Set("X-Forwarded-For", "192.0.2.1")
				req.Header.Set("CF-Connecting-IP", "192.0.2.1")
				req.Header.Set("X-Admin-Token", cfg.AdminToken)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, req)
				testutil.Falsef(t, rec.Code != http.StatusUnauthorized, "%s from %s must require a managed key: %d", prefix+suffix, source, rec.Code)
			}
		}
		req := httptest.NewRequest(http.MethodGet, prefix+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		testutil.Falsef(t, rec.Code != http.StatusOK, "managed key must reach %s/models: %d", prefix, rec.Code)
	}
}
