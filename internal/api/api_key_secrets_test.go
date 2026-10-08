package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func TestKeySecretLifecycleAndAdminAccess(t *testing.T) {
	s, mini := newTestStore(t, "secrets:")
	a := New(s, "admin", "pass", &config.Config{})
	create := httptest.NewRecorder()
	a.HandleKeys(create, httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"client","billing_limit_usd_ticks":1234,"billing_period_days":30}`)))
	if create.Code != 201 || create.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("create failed or cache allowed", create.Code)
	}
	var key CreateKeyResponse
	if err := json.Unmarshal(create.Body.Bytes(), &key); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("/api/keys/%d/secret", key.ID)
	handler := middleware.SessionAuthDynamic(func() (string, string) { return "admin-pass", "admin-token" }, a.HandleKeyByID)
	for _, token := range []string{"", key.Key, "admin-token"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		handler(rec, req)
		if token != "admin-token" {
			if rec.Code != 401 || strings.Contains(rec.Body.String(), key.Key) {
				t.Fatal("unauthorized secret access")
			}
			continue
		}
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Body.String(), key.Key) {
			t.Fatal("secret unavailable")
		}
	}
	raw, _ := mini.Get("secrets:api_keys:id:1")
	if strings.Contains(raw, key.Key) || !strings.Contains(raw, "encrypted_secret") {
		t.Fatal("plaintext persistence")
	}
	wrong, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "secrets:", CredentialEncryptionKey: []byte("abcdefghijklmnopqrstuvwx12345678")})
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	if _, err = wrong.GetApiKeySecret(context.Background(), key.ID); err == nil {
		t.Fatal("wrong master key decrypted secret")
	}
	list := httptest.NewRecorder()
	a.HandleKeys(list, httptest.NewRequest("GET", "/api/keys", nil))
	if strings.Contains(list.Body.String(), key.Key) || strings.Contains(list.Body.String(), "encrypted_secret") || !strings.Contains(list.Body.String(), `"secret_available":true`) {
		t.Fatal("unsafe list")
	}
	// A policy edit omits budget fields and must retain the existing ledger policy.
	patch := httptest.NewRecorder()
	a.HandleKeyByID(patch, httptest.NewRequest("PATCH", fmt.Sprintf("/api/keys/%d", key.ID), strings.NewReader(`{"rpm_limit":20}`)))
	rotated := httptest.NewRecorder()
	a.HandleKeyByID(rotated, httptest.NewRequest("POST", fmt.Sprintf("/api/keys/%d/rotate", key.ID), nil))
	if rotated.Code != 200 {
		t.Fatal(rotated.Body.String())
	}
	var next CreateKeyResponse
	json.Unmarshal(rotated.Body.Bytes(), &next)
	if _, err := s.AuthorizeApiKey(context.Background(), key.Key); err == nil {
		t.Fatal("old key still authenticates")
	}
	if _, err := s.AuthorizeApiKey(context.Background(), next.Key); err != nil {
		t.Fatal(err)
	}
	secret, err := s.GetApiKeySecret(context.Background(), key.ID)
	if err != nil || secret != next.Key {
		t.Fatal("rotation secret mismatch")
	}
	current, _ := s.GetApiKeyByID(context.Background(), key.ID)
	if current.BillingLimitUSDTicks != 1234 || current.BillingPeriodDays != 30 || current.RPMLimit != 20 {
		t.Fatal("rotation lost policy")
	}
	var record map[string]any
	raw, _ = mini.Get("secrets:api_keys:id:1")
	json.Unmarshal([]byte(raw), &record)
	record["encrypted_secret"] = "enc:v1:broken"
	bad, _ := json.Marshal(record)
	mini.Set("secrets:api_keys:id:1", string(bad))
	rec := httptest.NewRecorder()
	a.HandleKeyByID(rec, httptest.NewRequest("GET", url, nil))
	if rec.Code != 503 || strings.Contains(rec.Body.String(), next.Key) {
		t.Fatal("decryption failure did not fail closed")
	}
}

func TestLegacyKeysAndMissingCipher(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "legacy:"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	raw := "sk-legacy-secret"
	hash := sha256.Sum256([]byte(raw))
	key := &store.ApiKey{Name: "legacy", KeyHash: hex.EncodeToString(hash[:]), Enabled: true}
	if err = s.CreateApiKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	a := New(s, "admin", "pass", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleKeyByID(rec, httptest.NewRequest("GET", fmt.Sprintf("/api/keys/%d/secret", key.ID), nil))
	if rec.Code != 409 {
		t.Fatal("legacy must require rotation")
	}
	if _, err = s.AuthorizeApiKey(context.Background(), raw); err != nil {
		t.Fatal("legacy authentication changed", err)
	}
	rec = httptest.NewRecorder()
	a.HandleKeys(rec, httptest.NewRequest("POST", "/api/keys", strings.NewReader(`{"name":"new"}`)))
	if rec.Code != 500 {
		t.Fatal("missing encryption must reject creation")
	}
	rec = httptest.NewRecorder()
	a.HandleKeyByID(rec, httptest.NewRequest("POST", fmt.Sprintf("/api/keys/%d/rotate", key.ID), nil))
	if rec.Code != 500 {
		t.Fatal("missing encryption must reject rotation")
	}
	if _, err = s.AuthorizeApiKey(context.Background(), raw); err != nil {
		t.Fatal("failed rotation invalidated legacy")
	}
}
