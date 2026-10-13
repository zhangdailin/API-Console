package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

func TestRegisterRoutes_GrokConversationSurfacesAreRetired(t *testing.T) {
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin"}
	mux, s, _ := newRouteMux(t, "retired-grok-routes:", cfg)
	token := createRouteTestKey(t, s)

	for _, path := range []string{
		"/api/grok/tools/v1/models", "/api/grok/tools/v1/responses", "/api/grok/models",
		"/login", "/imagine", "/voice", "/video", "/v1/public", "/api/v1/public",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		testutil.CheckFalsef(t, rec.Code != http.StatusNotFound && rec.Code != http.StatusUnauthorized, "GET %s = %d, want an unregistered 404 or the /v1 auth guard's 401", path, rec.Code)
	}

	// Both legacy admin aliases are unregistered, including when an admin
	// credential is supplied. /v1 is guarded by inference auth, so use the
	// managed test key to observe its actual route result.
	for _, prefix := range []string{"/api/v1/admin", "/v1/admin"} {
		for _, suffix := range []string{"/verify", "/storage", "/config"} {
			path := prefix + suffix
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "192.0.2.1:1234"
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Admin-Token", cfg.AdminToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			testutil.CheckEqual(t, rec.Code, http.StatusNotFound)
		}
	}

	// The current list endpoint stays authenticated; the old whole-config
	// GET/POST endpoint is no longer registered.
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/api/config/list", nil)
		if authorized {
			req.Header.Set("X-Admin-Token", cfg.AdminToken)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if authorized {
			want = http.StatusOK
		}
		testutil.CheckEqual(t, rec.Code, want)
	}
	rec := httptest.NewRecorder()
	oldReq := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	oldReq.Header.Set("X-Admin-Token", cfg.AdminToken)
	mux.ServeHTTP(rec, oldReq)
	testutil.CheckEqual(t, rec.Code, http.StatusNotFound)

	// Local-only token-cache management no longer has a backing cache.
	for _, path := range []string{"/api/token-cache/stats", "/api/token-cache/clear"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Token", cfg.AdminToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		testutil.CheckEqual(t, rec.Code, http.StatusNotFound)
	}

	// These old management and media surfaces no longer have backing producers
	// or callers in the bundled console. The modern list/save, journal and alert
	// rules endpoints remain registered and are tested elsewhere.
	for _, path := range []string{
		"/api/grok/availability", "/api/models/groups", "/api/config/cache/clear",
		"/api/audit", "/api/ops/channels", "/api/ops/alerts",
		"/api/journal/operations", "/api/journal/system",
		"/admin/config", "/admin/cache", "/admin/token",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("X-Admin-Token", cfg.AdminToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		testutil.CheckEqual(t, rec.Code, http.StatusNotFound)
	}
	for _, path := range []string{"/grok/v1/files/image/missing.jpg", "/v1/files/video/missing.mp4"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = "192.0.2.1:1234"
		req.Header.Set("Authorization", "Bearer "+token) // test routing with a managed key
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		testutil.CheckEqual(t, rec.Code, http.StatusNotFound)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	testutil.Falsef(t, rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/", "GET / = %d location=%q, want 302 /admin/", rec.Code, rec.Header().Get("Location"))
}
