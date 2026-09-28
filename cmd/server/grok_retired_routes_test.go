package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
)

func TestRegisterRoutes_GrokConversationSurfacesAreRetired(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "retired-grok-routes:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin", AnonymousAllowIPs: []string{"192.0.2.1"}}
	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	t.Cleanup(h.Close)
	a := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, a, middleware.NewConcurrencyLimiter(4, 0, false), nil, renderer)

	for _, path := range []string{
		"/api/grok/tools/v1/models", "/api/grok/tools/v1/responses", "/api/grok/models",
		"/login", "/imagine", "/voice", "/video", "/v1/public", "/api/v1/public",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want an unregistered 404 or the /v1 auth guard's 401", path, rec.Code)
		}
	}

	// Both legacy admin aliases are unregistered, including when an admin
	// credential is supplied. /v1 is guarded by inference auth, so use the
	// configured anonymous test source to observe its actual route result.
	for _, prefix := range []string{"/api/v1/admin", "/v1/admin"} {
		for _, suffix := range []string{"/verify", "/storage", "/config"} {
			path := prefix + suffix
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = "192.0.2.1:1234"
			req.Header.Set("X-Admin-Token", cfg.AdminToken)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, rec.Code)
			}
		}
	}

	// The canonical config endpoint remains active behind session auth.
	for _, authorized := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
		if authorized {
			req.Header.Set("X-Admin-Token", cfg.AdminToken)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		want := http.StatusUnauthorized
		if authorized {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Errorf("GET /api/config (authorized=%v) = %d, want %d", authorized, rec.Code, want)
		}
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/admin/" {
		t.Fatalf("GET / = %d location=%q, want 302 /admin/", rec.Code, rec.Header().Get("Location"))
	}
}
