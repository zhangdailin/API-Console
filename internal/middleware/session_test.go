package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAPIKeyAuth(t *testing.T) {
	tests := []struct {
		name       string
		enabled    bool
		token      string
		valid      bool
		validate   error
		wantCode   int
		wantCalled bool
	}{
		{name: "disabled", enabled: false, wantCode: http.StatusNoContent, wantCalled: true},
		{name: "missing", enabled: true, wantCode: http.StatusUnauthorized},
		{name: "invalid", enabled: true, token: "bad", wantCode: http.StatusUnauthorized},
		{name: "backend unavailable", enabled: true, token: "key", validate: errors.New("redis down"), wantCode: http.StatusServiceUnavailable},
		{name: "valid", enabled: true, token: "key", valid: true, wantCode: http.StatusNoContent, wantCalled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := APIKeyAuthWithRequest(func(*http.Request) bool { return tt.enabled }, func(_ context.Context, token string) (*APIKeyPrincipal, error) {
				if token != tt.token {
					t.Fatalf("token=%q want=%q", token, tt.token)
				}
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
			if rec.Code != tt.wantCode || called != tt.wantCalled {
				t.Fatalf("status=%d called=%v want status=%d called=%v", rec.Code, called, tt.wantCode, tt.wantCalled)
			}
		})
	}
}

func TestAPIKeyAuthAcceptsAnthropicHeader(t *testing.T) {
	called := false
	h := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(_ context.Context, token string) (*APIKeyPrincipal, error) {
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
	if rec.Code != http.StatusNoContent || !called {
		t.Fatalf("status=%d called=%v", rec.Code, called)
	}
}

func TestAPIKeyAuthAddsNonSecretFingerprint(t *testing.T) {
	const token = "sk-sensitive"
	h := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(_ context.Context, got string) (*APIKeyPrincipal, error) {
		if got == token {
			return &APIKeyPrincipal{}, nil
		}
		return nil, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		fingerprint := APIKeyFingerprint(r.Context())
		if fingerprint == "" || strings.Contains(fingerprint, token) {
			t.Fatalf("unsafe fingerprint %q", fingerprint)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
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
			h := APIKeyAuthWithRequest(func(*http.Request) bool { return true }, func(context.Context, string) (*APIKeyPrincipal, error) {
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
			if rec.Code != tt.wantCode {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tt.wantCode, rec.Body.String())
			}
			if (rec.Header().Get("Retry-After") != "") != tt.wantRetry {
				t.Fatalf("Retry-After=%q", rec.Header().Get("Retry-After"))
			}
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

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
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
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status=%d want=%d", rec.Code, http.StatusUnauthorized)
			}
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
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status=%d want=%d", rec.Code, http.StatusNoContent)
			}
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

	if called {
		t.Fatalf("expected handler not to be called")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusUnauthorized)
	}
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

	if !called {
		t.Fatalf("expected handler to be called")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want=%d", rec.Code, http.StatusOK)
	}
}
