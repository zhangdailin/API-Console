package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestAPIKeyAuth(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		valid      bool
		validate   error
		wantCode   int
		wantCalled bool
	}{
		{name: "missing", wantCode: http.StatusUnauthorized},
		{name: "invalid", token: "bad", wantCode: http.StatusUnauthorized},
		{name: "backend unavailable", token: "key", validate: errors.New("redis down"), wantCode: http.StatusServiceUnavailable},
		{name: "valid", token: "key", valid: true, wantCode: http.StatusNoContent, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := APIKeyAuth(func(_ context.Context, token string) (*APIKeyPrincipal, error) {
				testutil.Equal(t, token, tt.token)
				if !tt.valid {
					return nil, tt.validate
				}
				return &APIKeyPrincipal{}, tt.validate
			}, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			if tt.token != "" {
				req.Header.Set("Authorization", "Bearer "+tt.token)
			}
			rec := httptest.NewRecorder()
			h(rec, req)
			testutil.Equal(t, rec.Code, tt.wantCode)
			testutil.Equal(t, called, tt.wantCalled)
		})
	}
}

func TestAPIKeyAuthAcceptsAnthropicHeader(t *testing.T) {
	called := false
	h := APIKeyAuth(func(_ context.Context, token string) (*APIKeyPrincipal, error) {
		if token == "anthropic-key" {
			return &APIKeyPrincipal{}, nil
		}
		return nil, nil
	}, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/messages", nil)
	req.Header.Set("x-api-key", "anthropic-key")
	rec := httptest.NewRecorder()
	h(rec, req)
	testutil.Falsef(t, rec.Code != http.StatusNoContent || !called, "status=%d called=%v", rec.Code, called)
}

func TestAPIKeyAuthAddsNonSecretFingerprint(t *testing.T) {
	const token = "sk-sensitive"
	h := APIKeyAuth(func(_ context.Context, got string) (*APIKeyPrincipal, error) {
		if got == token {
			return &APIKeyPrincipal{}, nil
		}
		return nil, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		fingerprint := APIKeyFingerprint(r.Context())
		testutil.Falsef(t, fingerprint == "" || strings.Contains(fingerprint, token), "unsafe fingerprint %q", fingerprint)
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	testutil.Equal(t, rec.Code, http.StatusNoContent)
}

func TestAPIKeyAuthEnforcesDenialsAndModelPolicy(t *testing.T) {
	tests := []struct {
		name      string
		principal *APIKeyPrincipal
		wantCode  int
		wantRetry bool
	}{
		{name: "expired", principal: &APIKeyPrincipal{DenialCode: APIKeyDenialExpired}, wantCode: http.StatusUnauthorized},
		{name: "rate limited", principal: &APIKeyPrincipal{DenialCode: APIKeyDenialRateLimited}, wantCode: http.StatusTooManyRequests, wantRetry: true},
		{name: "model denied", principal: &APIKeyPrincipal{AllowedModels: []string{"grok-4.6"}}, wantCode: http.StatusForbidden},
		{name: "model allowed case insensitive", principal: &APIKeyPrincipal{AllowedModels: []string{"GROK-4.5"}}, wantCode: http.StatusNoContent},
		{name: "wildcard", principal: &APIKeyPrincipal{AllowedModels: []string{"*"}}, wantCode: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := APIKeyAuth(func(context.Context, string) (*APIKeyPrincipal, error) {
				return tt.principal, nil
			}, func(w http.ResponseWriter, r *http.Request) {
				if !APIKeyAllowsModel(r.Context(), "grok-4.5") {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set("Authorization", "Bearer key")
			rec := httptest.NewRecorder()
			h(rec, req)
			testutil.Equal(t, rec.Code, tt.wantCode)
			testutil.Falsef(t, (rec.Header().Get("Retry-After") != "") != tt.wantRetry, "Retry-After=%q", rec.Header().Get("Retry-After"))
		})
	}
}

func TestSessionAuth_AdminPassBearer(t *testing.T) {
	called := false
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "" }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer admin123")
	rec := httptest.NewRecorder()
	handler(rec, req)

	testutil.True(t, called, "expected handler to be called")
	testutil.Equal(t, rec.Code, http.StatusOK)
}

func TestSessionAuth_RejectsLegacyQueryCredentials(t *testing.T) {
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "admintoken" }, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("legacy query credentials must not authorize admin requests")
	})

	for _, query := range []string{"app_key=admin123", "app_key=admintoken", "public_key=admin123", "public_key=admintoken"} {
		t.Run(query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/config?"+query, nil)
			rec := httptest.NewRecorder()
			handler(rec, req)
			testutil.Equal(t, rec.Code, http.StatusUnauthorized)
		})
	}
}

func TestSessionAuth_PreservesHeaderAndBasicAuth(t *testing.T) {
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "admintoken" }, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	for _, tt := range []struct {
		name  string
		setup func(*http.Request)
	}{
		{"admin bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer admintoken") }},
		{"admin raw", func(r *http.Request) { r.Header.Set("Authorization", "admintoken") }},
		{"admin header", func(r *http.Request) { r.Header.Set("X-Admin-Token", "admintoken") }},
		{"basic password", func(r *http.Request) { r.SetBasicAuth("admin", "admin123") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
			tt.setup(req)
			rec := httptest.NewRecorder()
			handler(rec, req)
			testutil.Equal(t, rec.Code, http.StatusNoContent)
		})
	}
}

func TestSessionAuth_Unauthorized(t *testing.T) {
	called := false
	handler := SessionAuthDynamic(func() (string, string) { return "admin123", "admintoken" }, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	testutil.False(t, called, "expected handler not to be called")
	testutil.Equal(t, rec.Code, http.StatusUnauthorized)
}

func TestSessionAuthDynamic_UsesLatestCredentials(t *testing.T) {
	called := false
	adminPass := "old-pass"
	handler := SessionAuthDynamic(func() (string, string) {
		return adminPass, ""
	}, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	adminPass = "new-pass"
	req := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req.Header.Set("Authorization", "Bearer new-pass")
	rec := httptest.NewRecorder()
	handler(rec, req)

	testutil.True(t, called, "expected handler to be called")
	testutil.Equal(t, rec.Code, http.StatusOK)
}
