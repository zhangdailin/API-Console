package grok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// responsesBridgeFixture builds a handler whose Build upstream answers with the
// supplied status, plus a verified model record so the Build catalog gate
// lets the request through (exactly as a real model refresh leaves it).
func responsesBridgeFixture(t *testing.T, upstreamStatus int, upstreamBody, model string) *Handler {
	t.Helper()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(upstreamStatus)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	t.Cleanup(upstream.Close)

	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisDB: 0, RedisPrefix: "bridge:"})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType:    "grok",
		CredentialType: "oauth", OAuthAccessToken: "build-token",
		Enabled:      true,
		Subscription: "super",
		Weight:       1,
		AgentMode:    model,
		GrokModels:   []string{model},
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	if err := s.CreateModel(ctx, &store.Model{
		Channel:       "grok",
		ModelID:       model,
		Name:          model,
		Status:        store.ModelStatusAvailable,
		Verified:      true,
		Provider:      ProviderBuild,
		UpstreamModel: model,
		Origin:        "discovery",
		Capabilities:  []string{store.CapabilityChat, store.CapabilityMessages, store.CapabilityResponses},
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	return NewHandler(&config.Config{GrokCLIBaseURL: upstream.URL}, loadbalancer.NewWithCacheTTL(s, 0))
}

// TestHandleResponses_RelaysUpstreamFailureStatus pins the native Responses
// error contract: an upstream credential failure is the operator-owned pool's
// problem (503), with a stable error envelope and no upstream body disclosure.
func TestHandleResponses_RelaysUpstreamFailureStatus(t *testing.T) {
	const sensitiveBody = `{"error":{"message":"OAuth token is invalid; team=bridge-private-team token=bridge-secret-token; https://x.ai/private-diagnostics"}}`
	h := responsesBridgeFixture(t, http.StatusUnauthorized, sensitiveBody, "grok-4.6")

	body := `{"model":"grok-4.6","input":"hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.HandleResponses(rec, req)

	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)
	got := rec.Header().Get("Content-Type")
	testutil.Falsef(t, !strings.HasPrefix(got, "application/json"), "Content-Type = %q, want application/json", got)
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &payload)
	testutil.CheckNoError(t, err)
	testutil.Equal(t, payload.Error.Code, "upstream_error")
	testutil.Equal(t, payload.Error.Type, "server_error")
	const wantMessage = "The upstream account session has expired. Re-authenticate the account and retry."
	testutil.Equal(t, payload.Error.Message, wantMessage)
	for _, leak := range []string{sensitiveBody, "OAuth token is invalid", "bridge-private-team", "bridge-secret-token", "x.ai/private-diagnostics", "status=", "body="} {
		testutil.CheckNotContain(t, rec.Body.String(), leak)
	}
}

// TestHandleResponses_ForbiddenModelIsNotServerError keeps a model-permission
// problem a 4xx for the Responses endpoint as well, matching chat completions.
func TestHandleResponses_ForbiddenModelIsNotServerError(t *testing.T) {
	h := responsesBridgeFixture(t, http.StatusOK, `{}`, "grok-4.6")

	body := `{"model":"grok-4.6","input":"hello","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// A key restricted to another model must be refused with 403, never 500.
	rec := httptest.NewRecorder()

	wrapped := middleware.APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: 1, AllowedModels: []string{"grok-4.5"}}, nil
	}, h.HandleResponses)
	req.Header.Set("Authorization", "Bearer test-key")
	wrapped(rec, req)

	testutil.Equal(t, rec.Code, http.StatusForbidden)
}
