package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"encoding/json"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func setupConfigAPI(t *testing.T) (*API, *store.Store, *miniredis.Miniredis) {
	t.Helper()

	s, mini := newTestStore(t, "test:")

	cfg := &config.Config{
		AdminPass:     "initial-secret",
		AdminToken:    "initial-token",
		CacheStrategy: "auto",
		ProxyURL:      "http://127.0.0.1:7890",
		ProxyBypass:   []string{"example.com"},
	}
	config.ApplyDefaults(cfg)

	return New(s, "admin", "pass", cfg), s, mini
}

func TestHandleConfigListReturnsCodeFreeMaxShape(t *testing.T) {
	api, _, _ := setupConfigAPI(t)

	req := httptest.NewRequest(http.MethodGet, "/api/config/list", nil)
	rec := httptest.NewRecorder()
	api.HandleConfigList(rec, req)

	var resp struct {
		Code int                    `json:"code"`
		Data map[string]interface{} `json:"data"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "unmarshal response: %v")

	testutil.Equal(t, resp.Code, 0)
	testutil.Equal(t, resp.Data["admin_pass"], "initial-secret")
	testutil.Equal(t, resp.Data["admin_password"], "initial-secret")
	_, ok := resp.Data["admin_token"]
	testutil.False(t, ok, "config list must not expose admin_token")
	testutil.Equal(t, resp.Data["cache_strategy"], "auto")
	for _, retired := range []string{"enable_token_cache", "token_cache_ttl", "token_cache_strategy", "cache_token_count", "cache_ttl"} {
		_, present := resp.Data[retired]
		testutil.Falsef(t, present, "retired field %s exposed by config list", retired)
	}
	testutil.Equal(t, resp.Data["proxy_url"], "http://127.0.0.1:7890")
}

func TestBuildConfigFromPatchRejectsAdminToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(`{"admin_token":"browser-controlled"}`))
	_, err := buildConfigFromPatch(req, &config.Config{AdminToken: "deployment-secret"})
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "deployment-managed"), "buildConfigFromPatch() error=%v, want deployment-managed rejection", err)
}

func TestHandleConfigSaveRejectsRetiredLocalCacheFields(t *testing.T) {
	api, s, _ := setupConfigAPI(t)
	for _, field := range []string{"enable_token_cache", "token_cache_ttl", "token_cache_strategy", "cache_token_count", "cache_ttl"} {
		t.Run(field, func(t *testing.T) {
			value := `"1"`
			if field == "enable_token_cache" || field == "cache_token_count" {
				value = `true`
			}
			req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(`{"`+field+`":`+value+`}`))
			rec := httptest.NewRecorder()
			api.HandleConfigSave(rec, req)
			testutil.Falsef(t, rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), field), "retired %s: status=%d body=%s", field, rec.Code, rec.Body.String())
		})
	}
	saved, err := s.GetSetting(context.Background(), "config")
	testutil.Falsef(t, err != nil || saved != "", "rejected patch changed config setting: %q err=%v", saved, err)
}

func TestHandleConfigSaveAcceptsCodeFreeMaxStylePayload(t *testing.T) {
	api, s, _ := setupConfigAPI(t)

	body := `{
		"admin_password":"changed-secret",
		"cache_strategy":"disabled",
		"proxy_url":"socks5://user:pass@127.0.0.1:1080",
		"proxy_bypass":"example.com, internal.local",
		"qoder_queue_retry_interval_ms":"15000"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.HandleConfigSave(rec, req)

	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "unmarshal response: %v")

	testutil.Equal(t, resp.Code, 0)
	testutil.Equal(t, resp.Msg, "success")

	cfg := api.config.Load()
	testutil.False(t, cfg == nil, "config not stored")
	testutil.Equal(t, cfg.AdminPass, "changed-secret")
	testutil.Equal(t, cfg.CacheStrategy, "disabled")
	testutil.Equal(t, cfg.QoderQueueRetryIntervalMs, 15000)
	testutil.Equal(t, cfg.ProxyURL, "socks5://user:pass@127.0.0.1:1080")
	testutil.Falsef(t, len(cfg.ProxyBypass) != 2 || cfg.ProxyBypass[0] != "example.com" || cfg.ProxyBypass[1] != "internal.local", "ProxyBypass=%v want [example.com internal.local]", cfg.ProxyBypass)

	saved, err := s.GetSetting(context.Background(), "config")
	testutil.NoError(t, err, "GetSetting(config) error = %v")
	testutil.MustContain(t, saved, `"admin_pass":"changed-secret"`)
	testutil.MustContain(t, saved, `"qoder_queue_retry_interval_ms":15000`)
}

func TestHandleConfigSavePublishesImmutableSnapshot(t *testing.T) {
	api, _, _ := setupConfigAPI(t)

	original := api.config.Load()
	testutil.False(t, original == nil, "expected initial config")

	body := `{
		"proxy_url":"http://alice:secret@127.0.0.1:9090"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(body))
	rec := httptest.NewRecorder()
	api.HandleConfigSave(rec, req)

	var resp struct {
		Code int `json:"code"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "unmarshal response: %v")
	testutil.Equal(t, resp.Code, 0)

	testutil.Equal(t, original.ProxyURL, "http://127.0.0.1:7890")
	updated := api.config.Load()
	testutil.NotEqual(t, updated, original)
	testutil.Equal(t, updated.ProxyURL, "http://alice:secret@127.0.0.1:9090")
}

func TestHandleConfigSaveNotifiesRuntimeConsumers(t *testing.T) {
	api, _, _ := setupConfigAPI(t)

	var notified *config.Config
	api.SetConfigChangeHook(func(cfg *config.Config) { notified = cfg })
	req := httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(`{"proxy_url":"http://127.0.0.1:9091"}`))
	rec := httptest.NewRecorder()
	api.HandleConfigSave(rec, req)

	testutil.False(t, notified == nil, "runtime config hook was not called")
	testutil.Equal(t, notified, api.config.Load())
	testutil.Equal(t, notified.ProxyURL, "http://127.0.0.1:9091")
}

func TestPersistConfigConcurrentReadersSeeCompleteSnapshots(t *testing.T) {
	api, _, _ := setupConfigAPI(t)

	const updates = 100
	var readers sync.WaitGroup
	errCh := make(chan error, 8)
	done := make(chan struct{})
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				snapshot := api.config.Load()
				if snapshot == nil || snapshot.AdminUser == "admin" {
					continue
				}
				if snapshot.ProxyURL != "http://"+snapshot.AdminUser+".example" {
					select {
					case errCh <- fmt.Errorf("torn snapshot: user=%q proxy=%q", snapshot.AdminUser, snapshot.ProxyURL):
					default:
					}
					return
				}
			}
		}()
	}

	for i := range updates {
		current := api.config.Load()
		next := current.Clone()
		next.AdminUser = fmt.Sprintf("admin-%d", i)
		next.ProxyURL = "http://" + next.AdminUser + ".example"
		if err := api.persistConfig(context.Background(), current, next); err != nil {
			close(done)
			readers.Wait()
			t.Fatalf("persistConfig: %v", err)
		}
	}
	close(done)
	readers.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

// A signing endpoint is validated where it is typed: the value decides whether
// account page metadata leaves the host, so an invalid one must not be stored.
func TestPersistConfigValidatesAnonymousAllowIPs(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()

	broken := &config.Config{AnonymousAllowIPs: []string{"10.0.0.0/8", "not-an-address"}}
	err := a.persistConfig(ctx, nil, broken)
	testutil.Error(t, err)
	saved, err := s.GetSetting(ctx, "config")
	testutil.Falsef(t, err != nil || strings.Contains(saved, "not-an-address"), "the rejected value reached the store: %q err=%v", saved, err)

	valid := &config.Config{AnonymousAllowIPs: []string{"203.0.113.20", "198.51.100.0/24"}}
	testutil.NoError(t, a.persistConfig(ctx, nil, valid), "a valid allowlist was rejected: %v")
}

func TestPersistConfigRejectsMalformedProxy(t *testing.T) {
	a, s, cleanup := newTestAPI(t)
	defer cleanup()
	ctx := context.Background()

	broken := &config.Config{ProxyURL: "ftp://proxy.local:3128"}
	err := a.persistConfig(ctx, nil, broken)
	testutil.Error(t, err)
	saved, getErr := s.GetSetting(ctx, "config")
	testutil.Falsef(t, getErr != nil || strings.Contains(saved, "ftp://proxy.local"), "invalid proxy reached the store: %q err=%v", saved, getErr)
}

func TestHandleConfigSaveRejectsProxyWithoutPublishingOrPersisting(t *testing.T) {
	for _, field := range []string{"proxy_url", "proxy_http", "proxy_https"} {
		t.Run(field, func(t *testing.T) {
			a, s, _ := setupConfigAPI(t)
			ctx := context.Background()
			before := a.ConfigSnapshot()
			testutil.NoError(t, s.SetSetting(ctx, "config", "unchanged"))
			hookCalled := false
			a.SetConfigChangeHook(func(*config.Config) { hookCalled = true })
			patch := map[string]string{field: "http://proxy-user:proxy-secret@proxy.local:%zz"}
			body, err := json.Marshal(patch)
			testutil.NoError(t, err)
			rec := httptest.NewRecorder()
			a.HandleConfigSave(rec, httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(string(body))))
			var envelope struct {
				Code int `json:"code"`
			}
			testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
			testutil.NotEqual(t, envelope.Code, 0)
			testutil.MustNotContainAny(t, rec.Body.String(), "proxy-user", "proxy-secret")
			testutil.Equal(t, a.ConfigSnapshot(), before)
			testutil.False(t, hookCalled, "invalid config must not invoke the publication hook")
			saved, err := s.GetSetting(ctx, "config")
			testutil.NoError(t, err)
			testutil.Equal(t, saved, "unchanged")
		})
	}
}
