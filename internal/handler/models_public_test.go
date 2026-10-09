package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestHandleModels_FiltersAPIKeyModelAllowlist(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	// The catalog is published explicitly: the store starts empty now.
	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.6"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-image"},
	)

	wrapper := middleware.APIKeyAuthWithRequest(
		func(*http.Request) bool { return true },
		func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
			return &middleware.APIKeyPrincipal{AllowedModels: []string{"grok-4.6"}}, nil
		},
		h.HandleModels,
	)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	rec := httptest.NewRecorder()
	wrapper(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	body := rec.Body.String()
	testutil.Falsef(t, !strings.Contains(body, "grok-4.6") || strings.Contains(body, "grok-4.5") || strings.Contains(body, "grok-imagine-image"), "unexpected filtered models: %s", body)
}

func TestHandleModelByID_HidesOfflineModel(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel: "WorkBuddy",
		ModelID: "offline-only-model",
		Name:    "Offline Only",
		Status:  store.ModelStatusOffline,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/workbuddy/v1/models/offline-only-model", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	testutil.Equal(t, rec.Code, http.StatusNotFound)
}

func TestHandleModelByID_HidesUnsupportedGrokModel(t *testing.T) {
	h, _, _ := setupModelValidationHandler(t)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-4.1", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	testutil.Equal(t, rec.Code, http.StatusNotFound)
}

func TestHandleModelByID_ReturnsVisibleModel(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	publishModel(t, s, &store.Model{Channel: "Grok", ModelID: "grok-4.5"})
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=super-token",
		Subscription: "super",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-4.5", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
}

func TestHandleModelByID_ReturnsVerifiedDynamicGrokModel(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	if err := s.CreateModel(context.Background(), &store.Model{
		Channel:  "Grok",
		ModelID:  "grok-future-6",
		Name:     "grok-future-6",
		Status:   store.ModelStatusAvailable,
		Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel() error = %v", err)
	}
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-future-6", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
}

func TestHandleModels_KeepsGrokModelsVisibleWhenOnlyBasicPoolExists(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-image"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-video"},
	)
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	body := rec.Body.String()
	testutil.MustContain(t, body, "grok-4.5")
	testutil.MustContainAll(t, body, "grok-imagine-image", "grok-imagine-video")
}

func TestHandleModels_KeepsGrokModelsVisibleWhenAccountsHaveStatusCode(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	// Visibility follows verified observations, independent of historical names.
	publishModel(t, s,
		&store.Model{Channel: "Grok", ModelID: "grok-4.5"},
		&store.Model{Channel: "Grok", ModelID: "grok-imagine-image"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.20-0309-non-reasoning"},
		&store.Model{Channel: "Grok", ModelID: "grok-4.3-beta"},
		&store.Model{Channel: "Grok", ModelID: "grok-build-0.1"},
	)
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=super-token",
		Subscription: "super",
		Enabled:      true,
		StatusCode:   "500",
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models", nil)
	rec := httptest.NewRecorder()

	h.HandleModels(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	body := rec.Body.String()
	testutil.MustContain(t, body, "grok-4.5")
	testutil.MustContain(t, body, "grok-imagine-image")
	for _, observed := range []string{"grok-4.20-0309-non-reasoning", "grok-4.3-beta", "grok-build-0.1"} {
		testutil.MustContain(t, body, `"id":"`+observed+`"`)
	}
}

func TestHandleModelByID_ReturnsGrokModelWithoutRequiredPool(t *testing.T) {
	h, s, _ := setupModelValidationHandler(t)

	publishModel(t, s, &store.Model{Channel: "Grok", ModelID: "grok-imagine-video"})
	if err := s.CreateAccount(context.Background(), &store.Account{
		AccountType:  "grok",
		ClientCookie: "sso=basic-token",
		Subscription: "basic",
		Enabled:      true,
	}); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/grok/v1/models/grok-imagine-video", nil)
	rec := httptest.NewRecorder()

	h.HandleModelByID(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
}
