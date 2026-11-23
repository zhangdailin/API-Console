package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestConfigPatchesAcrossReplicasPreserveUnrelatedChanges(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	cfg := a.ConfigSnapshot()
	b := New(s, cfg.AdminUser, cfg.AdminPass, cfg.Clone())
	write := func(api *API, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		api.HandleConfigSave(w, httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(body)))
		resp := struct{ Code int }{Code: -1}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != http.StatusOK || resp.Code != 0 {
			t.Fatalf("config rejected: status=%d code=%d decode=%v", w.Code, resp.Code, err)
		}
	}
	stored := func() *config.Config {
		t.Helper()
		raw, err := s.GetSetting(t.Context(), "config")
		if err != nil {
			t.Fatal("read config:", err)
		}
		cfg := &config.Config{}
		if err := json.Unmarshal([]byte(raw), cfg); err != nil {
			t.Fatal("decode config:", err)
		}
		return cfg
	}
	write(a, `{"debug_enabled":true}`)
	if !stored().DebugEnabled || !a.ConfigSnapshot().DebugEnabled {
		t.Fatal("first patch did not take effect")
	}
	write(b, `{"proxy_bypass":["replica-b.test"]}`)
	final := stored()
	if !final.DebugEnabled || !slices.Equal(final.ProxyBypass, []string{"replica-b.test"}) || !a.ConfigSnapshot().DebugEnabled || !b.ConfigSnapshot().DebugEnabled {
		t.Fatal("both committed patches must be present in Redis and on the writing replica")
	}
	if err := a.syncSavedConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.ConfigSnapshot().ProxyBypass, final.ProxyBypass) {
		t.Fatal("the other replica did not adopt the committed configuration")
	}
}

func TestAPIKeyRotationPreservesStoredResponses(t *testing.T) {
	s, _ := newTestStore(t, "audit4-rotation:")
	const before = "sk-fixture-before-rotation"
	const after = "sk-fixture-after-rotation"
	digest := sha256.Sum256([]byte(before))
	key := &store.ApiKey{Name: "test", KeyHash: hex.EncodeToString(digest[:]), Enabled: true}
	if err := s.CreateApiKey(t.Context(), key); err != nil {
		t.Fatal("create key:", err)
	}
	authorize := func(ctx context.Context, raw string) (*middleware.APIKeyPrincipal, error) {
		row, err := s.AuthorizeApiKey(ctx, raw)
		if errors.Is(err, store.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &middleware.APIKeyPrincipal{ID: row.ID, ResourceOwner: row.ResourceOwner}, nil
	}
	owner := func(raw string) (string, int64, int) {
		var scope string
		var id int64
		handler := middleware.APIKeyAuth(authorize, func(w http.ResponseWriter, r *http.Request) {
			scope = middleware.APIKeyFingerprint(r.Context())
			id = middleware.APIKeyID(r.Context())
			w.WriteHeader(http.StatusNoContent)
		})
		r := httptest.NewRequest(http.MethodGet, "/grok/v1/responses/resp_audit4", nil)
		r.Header.Set("Authorization", "Bearer "+raw)
		w := httptest.NewRecorder()
		handler(w, r)
		return scope, id, w.Code
	}
	oldScope, oldID, code := owner(before)
	if oldID == 0 || oldScope == "" || code != http.StatusNoContent {
		t.Fatal("initial key not authenticated")
	}
	row := &store.StoredResponse{ResponseID: "resp_audit4", OwnerHash: oldScope, AccountID: 1, Model: "fixture", Provider: "grok", Body: []byte("{}")}
	if err := s.SaveStoredResponse(t.Context(), row, time.Minute); err != nil {
		t.Fatal("save response:", err)
	}
	if _, err := s.GetStoredResponse(t.Context(), row.ResponseID, oldScope); err != nil {
		t.Fatal("pre-rotation response unavailable:", err)
	}
	if _, err := s.RotateApiKey(t.Context(), key.ID, after); err != nil {
		t.Fatal("rotate key:", err)
	}
	newScope, newID, newCode := owner(after)
	_, _, oldCode := owner(before)
	if newID != oldID || newScope == "" || newScope != oldScope || newCode != http.StatusNoContent || oldCode != http.StatusUnauthorized {
		t.Fatal("expected same key record under the new secret only, with an unchanged resource scope")
	}
	if _, err := s.GetStoredResponse(t.Context(), row.ResponseID, newScope); err != nil {
		t.Fatalf("resource missing after rotation: %v", err)
	}
	const other = "sk-fixture-separate-tenant"
	otherHash := sha256.Sum256([]byte(other))
	if err := s.CreateApiKey(t.Context(), &store.ApiKey{KeyHash: hex.EncodeToString(otherHash[:]), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	otherScope, otherID, otherCode := owner(other)
	if otherID == newID || otherCode != http.StatusNoContent {
		t.Fatal("other key did not authenticate independently")
	}
	if _, err := s.GetStoredResponse(t.Context(), row.ResponseID, otherScope); !errors.Is(err, store.ErrNoRows) {
		t.Fatal("another key gained access to the resource")
	}
	const second = "sk-fixture-second-rotation"
	if _, err := s.RotateApiKey(t.Context(), newID, second); err != nil {
		t.Fatal(err)
	}
	if scope, _, code := owner(second); scope != oldScope || code != http.StatusNoContent {
		t.Fatal("a second rotation changed the owning resource scope")
	}
}

func TestModelCooldownCannotRestoreOldAdminAccountState(t *testing.T) {
	s, _ := newTestStore(t, "audit4-account-state:")
	acc := &store.Account{AccountType: "grok", Name: "old name", Weight: 1, Enabled: true, ClientCookie: "fixture-old-cookie"}
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal("create account:", err)
	}
	get := func() *store.Account {
		t.Helper()
		got, err := s.GetAccount(t.Context(), acc.ID)
		if err != nil {
			t.Fatal("get account:", err)
		}
		return got
	}
	inflight := get()
	admin := get()
	admin.Enabled, admin.Name, admin.Weight, admin.ClientCookie = false, "revised name", 4, "fixture-new-cookie"
	if err := s.UpdateAccount(t.Context(), admin); err != nil {
		t.Fatal("persist admin update:", err)
	}
	current := get()
	if current.Enabled || current.ClientCookie != admin.ClientCookie || !current.UpdatedAt.After(inflight.UpdatedAt) {
		t.Fatal("control: newer administrative change is missing")
	}
	if err := s.SetModelCooldown(t.Context(), inflight, "fixture-model", time.Now().Add(time.Minute), store.ModelCooldownThrottled); err != nil {
		t.Fatal("persist model cooldown:", err)
	}
	final := get()
	if final.Enabled || final.Name != admin.Name || final.Weight != admin.Weight || final.ClientCookie != admin.ClientCookie || len(final.ModelCooldowns) != 0 {
		t.Fatal("a stale cooldown changed administrator settings or applied to the new credential")
	}
	final.Enabled = true
	if err := s.UpdateAccount(t.Context(), final); err != nil {
		t.Fatal(err)
	}
	final = get()
	if err := s.SetModelCooldown(t.Context(), final, "fixture-model", time.Now().Add(time.Minute), store.ModelCooldownUnavailable); err != nil {
		t.Fatal(err)
	}
	if got := store.ModelCooldownKind(get(), "fixture-model", time.Now()); got != store.ModelCooldownUnavailable {
		t.Fatalf("current credential's model verdict was not saved: %q", got)
	}
}

func TestConfigCompareAndSaveRejectsAConcurrentPreimageChange(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	cfg := a.ConfigSnapshot()
	b := New(s, cfg.AdminUser, cfg.AdminPass, cfg.Clone())
	err := a.mutateSavedConfig(t.Context(), func(base *config.Config) (*config.Config, error) {
		rec := httptest.NewRecorder()
		b.HandleConfigSave(rec, httptest.NewRequest(http.MethodPost, "/api/config/save", strings.NewReader(`{"proxy_bypass":["other.test"]}`)))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":0`) {
			t.Fatal("second replica failed to commit")
		}
		next := base.Clone()
		next.DebugEnabled = true
		return next, nil
	})
	if !errors.Is(err, store.ErrConfigConflict) {
		t.Fatalf("expected concurrent save to fail with a conflict: %v", err)
	}
	if err := a.syncSavedConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	if cfg := a.ConfigSnapshot(); cfg.DebugEnabled || !slices.Equal(cfg.ProxyBypass, []string{"other.test"}) {
		t.Fatal("concurrent save lost the winning replica's configuration")
	}
}

func TestRevokedBrowserSessionCannotCommitAnAlreadyAuthorizedConfigPatch(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	backend := auth.NewRedisSessionBackend(s.RedisClient(), s.RedisPrefix())
	auth.SetSessionBackend(backend)
	t.Cleanup(func() { auth.SetSessionBackend(nil); auth.ClearLocalSessions() })
	base := a.ConfigSnapshot()
	if err := a.persistConfig(t.Context(), base, base); err != nil {
		t.Fatal(err)
	}
	token, err := auth.GenerateSessionTokenForPassword(base.AdminPass)
	if err != nil {
		t.Fatal(err)
	}
	b := New(s, base.AdminUser, base.AdminPass, base)
	handler := middleware.SessionAuthDynamic(func() (string, string) {
		return base.AdminPass, base.AdminToken
	}, func(w http.ResponseWriter, r *http.Request) {
		rotated := base.Clone()
		rotated.AdminPass = "fixture-rotated-admin-password"
		if err := b.persistConfig(t.Context(), base, rotated); err != nil {
			t.Fatal(err)
		}
		a.HandleConfigSave(w, r)
	})
	req := httptest.NewRequest(http.MethodPost, "https://admin.example.test/api/config/save", strings.NewReader(`{"debug_enabled":true}`))
	req.Header.Set("Origin", "https://admin.example.test")
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "session_token", Value: token})
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("session revoked between authentication and write committed or was misreported: HTTP %d", rec.Code)
	}
	if err := a.syncSavedConfig(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.ConfigSnapshot().DebugEnabled || a.ConfigSnapshot().AdminPass == base.AdminPass {
		t.Fatal("the stale browser session overwrote the winning configuration")
	}
}
