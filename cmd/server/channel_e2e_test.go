package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/api"
	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/template"
	"orchids-api/internal/testutil"
)

// channelE2E drives one channel through the real route table, handler and client
// running on a live listener.
type channelE2E struct {
	mux        *http.ServeMux
	server     *httptest.Server
	client     *http.Client
	managedKey string
}

func (e *channelE2E) do(t *testing.T, method, path, body string, admin bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, e.server.URL+path, strings.NewReader(body))
	testutil.NoError(t, err, "build request: %v")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.server.URL)
	if admin {
		req.Header.Set("X-Admin-Token", "admintoken")
	} else {
		req.Header.Set("Authorization", "Bearer "+e.managedKey)
	}
	resp, err := e.client.Do(req)
	testutil.Falsef(t, err != nil, "%s %s: %v", method, path, err)
	return resp
}

func (e *channelE2E) readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	testutil.NoError(t, err, "read body: %v")
	return string(raw)
}

// newRouteMux builds the real store, handler, API and registered route table on
// a miniredis instance, so a test drives the deployment's own wiring.
func newRouteMux(t *testing.T, redisPrefix string, cfg *config.Config) (*http.ServeMux, *store.Store, *handler.Handler) {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: redisPrefix})
	testutil.NoError(t, err, "store.New() error = %v")
	t.Cleanup(func() { _ = s.Close() })

	lb := loadbalancer.NewWithCacheTTL(s, 0)
	h := handler.NewWithLoadBalancer(cfg, lb)
	t.Cleanup(h.Close)

	apiHandler := api.New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	renderer, err := template.NewRenderer()
	testutil.NoError(t, err, "template.NewRenderer() error = %v")
	mux := http.NewServeMux()
	registerRoutes(mux, cfg, s, h, nil, apiHandler, middleware.NewConcurrencyLimiter(4, 0), nil, renderer)
	return mux, s, h
}

// newChannelE2E wires the route table plus the provider-table client factory onto
// a live listener, so a channel test travels the same lifecycle the deployment
// uses.
func newChannelE2E(t *testing.T, redisPrefix, managedKey string, cfg *config.Config) *channelE2E {
	t.Helper()
	mux, s, h := newRouteMux(t, redisPrefix, cfg)

	digest := sha256.Sum256([]byte(managedKey))
	if err := s.CreateApiKey(context.Background(), &store.ApiKey{
		Name: managedKey, KeyHash: hex.EncodeToString(digest[:]), KeyPrefix: "sk-", KeySuffix: "-e2e", Enabled: true,
	}); err != nil {
		t.Fatalf("CreateApiKey() error = %v", err)
	}

	h.SetClientFactory(func(acc *store.Account, c *config.Config) handler.UpstreamClient {
		spec, ok := provider.Lookup(acc.AccountType)
		testutil.True(t, ok && spec.NewClient != nil, "no inference client registered for %q")
		return spec.NewClient(acc, c, s)
	})

	// A real listener is used so request contexts live for the whole handler, as
	// they do in production.
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &channelE2E{
		mux:        mux,
		server:     server,
		client:     &http.Client{Timeout: 60 * time.Second},
		managedKey: managedKey,
	}
}
