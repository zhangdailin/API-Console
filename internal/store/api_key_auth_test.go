package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestAuthorizeApiKeyBasicValidation(t *testing.T) {
	s, mini := newTestRedisStore(t, "auth-test:")

	raw := "sk-test-secret"
	digest := sha256.Sum256([]byte(raw))
	key := &ApiKey{
		Name:      "test",
		KeyHash:   hex.EncodeToString(digest[:]),
		KeyPrefix: "sk-",
		KeySuffix: "cret",
		Enabled:   true,
	}
	testutil.NoError(t, s.CreateApiKey(context.Background(), key), "CreateApiKey() error = %v")

	authorized, err := s.AuthorizeApiKey(context.Background(), raw)
	testutil.Falsef(t, err != nil || authorized == nil, "AuthorizeApiKey(valid) = %#v, %v", authorized, err)
	persisted, err := s.GetApiKeyByID(context.Background(), key.ID)
	testutil.Falsef(t, err != nil || persisted.LastUsedAt == nil, "unlimited key last_used_at was not persisted: key=%#v err=%v", persisted, err)
	rawRecord, err := mini.Get("auth-test:api_keys:id:1")
	testutil.Falsef(t, err != nil || strings.Contains(rawRecord, raw) || strings.Contains(rawRecord, "key_full"), "Redis must not contain the plaintext key: record=%q err=%v", rawRecord, err)
	if _, err = s.AuthorizeApiKey(context.Background(), "sk-wrong"); !errors.Is(err, ErrNoRows) {
		t.Fatalf("AuthorizeApiKey(wrong) error = %v, want ErrNoRows", err)
	}
	key.Enabled = false
	testutil.NoError(t, s.UpdateApiKey(context.Background(), key), "UpdateApiKey() error = %v")
	if _, err = s.AuthorizeApiKey(context.Background(), raw); !errors.Is(err, ErrNoRows) {
		t.Fatalf("AuthorizeApiKey(disabled) error = %v, want ErrNoRows", err)
	}
}

func TestAuthorizeApiKeyPolicyAndRPM(t *testing.T) {
	s, _ := newTestRedisStore(t, "auth-policy-test:")

	raw := "sk-policy-secret"
	digest := sha256.Sum256([]byte(raw))
	key := &ApiKey{
		Name:          "policy",
		KeyHash:       hex.EncodeToString(digest[:]),
		KeyPrefix:     "sk-",
		KeySuffix:     "cret",
		Enabled:       true,
		AllowedModels: []string{"grok-4.6"},
		RPMLimit:      2,
	}
	testutil.NoError(t, s.CreateApiKey(context.Background(), key), "CreateApiKey() error = %v")

	for i := range 2 {
		got, err := s.AuthorizeApiKey(context.Background(), raw)
		testutil.Falsef(t, err != nil || got == nil || len(got.AllowedModels) != 1 || got.AllowedModels[0] != "grok-4.6", "AuthorizeApiKey(%d) = %#v, %v", i, got, err)
	}
	persisted, err := s.GetApiKeyByID(context.Background(), key.ID)
	testutil.Falsef(t, err != nil || persisted.LastUsedAt == nil, "last_used_at was not persisted: key=%#v err=%v", persisted, err)
	_, err = s.AuthorizeApiKey(context.Background(), raw)
	testutil.CheckEqual(t, err, ErrApiKeyRateLimited)
}

func TestAuthorizeApiKeyRejectsExpiredKey(t *testing.T) {
	s, _ := newTestRedisStore(t, "auth-expired-test:")

	raw := "sk-expired-secret"
	digest := sha256.Sum256([]byte(raw))
	expiresAt := time.Now().UTC().Add(-time.Minute)
	key := &ApiKey{
		Name:      "expired",
		KeyHash:   hex.EncodeToString(digest[:]),
		KeyPrefix: "sk-",
		KeySuffix: "cret",
		Enabled:   true,
		ExpiresAt: &expiresAt,
	}
	testutil.NoError(t, s.CreateApiKey(context.Background(), key), "CreateApiKey() error = %v")
	_, err := s.AuthorizeApiKey(context.Background(), raw)
	testutil.CheckEqual(t, err, ErrApiKeyExpired)
}

func TestUpdateApiKeyPolicyPersists(t *testing.T) {
	s, _ := newTestRedisStore(t, "auth-update-test:")

	key := &ApiKey{Name: "update", KeyHash: "hash", KeyPrefix: "sk-", KeySuffix: "hash", Enabled: true}
	testutil.NoError(t, s.CreateApiKey(context.Background(), key), "CreateApiKey() error = %v")
	expiresAt := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	key.AllowedModels = []string{"grok-4.6", "grok-imagine-video"}
	key.RPMLimit = 17
	key.ExpiresAt = &expiresAt
	testutil.NoError(t, s.UpdateApiKey(context.Background(), key), "UpdateApiKey() error = %v")
	got, err := s.GetApiKeyByID(context.Background(), key.ID)
	testutil.NoError(t, err, "GetApiKeyByID() error = %v")
	testutil.Falsef(t, got.RPMLimit != 17 || got.ExpiresAt == nil || !got.ExpiresAt.Equal(expiresAt) || len(got.AllowedModels) != 2, "persisted key = %#v", got)
}
