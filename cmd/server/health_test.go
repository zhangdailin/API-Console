package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

// TestHealthReportsEveryRemainingProvider pins /health's body: it answers "ok"
// and lists one entry per registered channel, so the probe can no longer report a
// provider this gateway does not serve. The set is asserted against the channel
// registry rather than a literal, which is what makes a removed provider
// disappear from the response automatically.
func TestHealthReportsEveryRemainingProvider(t *testing.T) {
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminPath: "/admin"}
	mux, _, _ := newRouteMux(t, "health:", cfg)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	testutil.Equal(t, rec.Code, http.StatusOK)
	for _, endpoint := range []string{"/api/ops/pools", "/api/system/version", "/api/system/check-updates", "/api/system/operation", "/api/system/update", "/api/system/rollback"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		mux.ServeHTTP(response, request)
		testutil.Equal(t, response.Code, http.StatusUnauthorized)
	}
	versionResponse := httptest.NewRecorder()
	versionRequest := httptest.NewRequest(http.MethodGet, "/api/system/version", nil)
	versionRequest.Header.Set("Authorization", "Bearer secret")
	mux.ServeHTTP(versionResponse, versionRequest)
	testutil.Equal(t, versionResponse.Code, http.StatusOK)
	var body struct {
		Status    string            `json:"status"`
		Providers map[string]string `json:"providers"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &body)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, body.Status, "ok")
	want := map[string]struct{}{}
	for _, definition := range channel.All() {
		want[string(definition.ID)] = struct{}{}
		testutil.Equal(t, body.Providers[string(definition.ID)], "ready")
	}
	testutil.Equal(t, len(body.Providers), len(want))
}
