package store

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestAccountCredentialsEncryptedAndLegacyMigratesOnWrite(t *testing.T) {
	mini := miniredis.RunT(t)
	key := bytes.Repeat([]byte{0x2a}, 32)
	s, err := New(Options{
		RedisAddr:               mini.Addr(),
		RedisPrefix:             "cipher-test:",
		CredentialEncryptionKey: key,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	acc := &Account{
		Name:                  "encrypted",
		AccountType:           "grok",
		Enabled:               true,
		ClientCookie:          "sso-secret",
		OAuthAccessToken:      "access-secret",
		OAuthRefreshToken:     "refresh-secret",
		WorkBuddyAccessToken:  "wb-access-secret",
		WorkBuddyRefreshToken: "wb-refresh-secret",
		QoderAccessToken:      "qoder-access-secret",
		QoderRefreshToken:     "qoder-refresh-secret",
		QoderRuntimeInfo:      "qoder-runtime-secret",
		QoderRuntimeKey:       "qoder-key-secret",
	}
	if err := s.CreateAccount(ctx, acc); err != nil {
		t.Fatalf("CreateAccount() error = %v", err)
	}
	raw, err := mini.Get("cipher-test:accounts:id:1")
	if err != nil {
		t.Fatalf("read raw account: %v", err)
	}
	for _, secret := range []string{
		"sso-secret", "access-secret", "refresh-secret", "wb-access-secret", "wb-refresh-secret",
		"qoder-access-secret", "qoder-refresh-secret", "qoder-runtime-secret", "qoder-key-secret", "qoder-job-secret",
	} {
		if strings.Contains(raw, secret) {
			t.Fatalf("raw Redis value contains plaintext %q: %s", secret, raw)
		}
	}
	if !strings.Contains(raw, encryptedCredentialPrefix) {
		t.Fatalf("raw Redis value does not contain encrypted fields: %s", raw)
	}
	got, err := s.GetAccount(ctx, acc.ID)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if got.ClientCookie != acc.ClientCookie || got.OAuthAccessToken != acc.OAuthAccessToken || got.OAuthRefreshToken != acc.OAuthRefreshToken {
		t.Fatalf("decrypted credentials mismatch: %#v", got)
	}
	if got.WorkBuddyRefreshToken != acc.WorkBuddyRefreshToken || got.QoderRefreshToken != acc.QoderRefreshToken ||
		got.QoderRuntimeKey != acc.QoderRuntimeKey {
		t.Fatalf("decrypted provider credentials mismatch: %#v", got)
	}

	legacy := `{"id":2,"name":"legacy","account_type":"grok","enabled":true,"client_cookie":"legacy-secret"}`
	mini.Set("cipher-test:accounts:id:2", legacy)
	mini.SAdd("cipher-test:accounts:ids", "2")
	legacyAcc, err := s.GetAccount(ctx, 2)
	if err != nil || legacyAcc.ClientCookie != "legacy-secret" {
		t.Fatalf("legacy read = %#v, %v", legacyAcc, err)
	}
	if err := s.UpdateAccount(ctx, legacyAcc); err != nil {
		t.Fatalf("legacy migration update error = %v", err)
	}
	migrated, _ := mini.Get("cipher-test:accounts:id:2")
	if strings.Contains(migrated, "legacy-secret") || !strings.Contains(migrated, encryptedCredentialPrefix) {
		t.Fatalf("legacy account was not encrypted on write: %s", migrated)
	}
}

// Retired account metadata is intentionally dropped even when an old Redis row
// still carries it; only the live provider credentials survive a rewrite.
func TestRetiredAccountFieldsAreDroppedOnRewrite(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "retired-fields:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	mini.Set("retired-fields:accounts:id:1", `{"id":1,"name":"legacy","account_type":"workbuddy","enabled":true,"nsfw_enabled":true,"device_id":"old-device","request_id":"old-request","project_id":"old-project","upstream_mode":"old-mode","workbuddy_refresh_token":"live-refresh"}`)
	mini.SAdd("retired-fields:accounts:ids", "1")
	acc, err := s.GetAccount(ctx, 1)
	if err != nil || acc.WorkBuddyRefreshToken != "live-refresh" {
		t.Fatalf("GetAccount() = %+v, %v", acc, err)
	}
	acc.Name = "rewritten"
	if err := s.UpdateAccount(ctx, acc); err != nil {
		t.Fatal(err)
	}
	raw, err := mini.Get("retired-fields:accounts:id:1")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"nsfw_enabled", "device_id", "request_id", "project_id", "upstream_mode"} {
		if strings.Contains(raw, `"`+field+`"`) {
			t.Errorf("retired field %q survived rewrite: %s", field, raw)
		}
	}
	if !strings.Contains(raw, `"workbuddy_refresh_token":"live-refresh"`) {
		t.Errorf("live WorkBuddy refresh token lost: %s", raw)
	}
}

func TestEncryptedAccountRejectsWrongKey(t *testing.T) {
	cipherA, _ := newCredentialCipher(bytes.Repeat([]byte{1}, 32))
	cipherB, _ := newCredentialCipher(bytes.Repeat([]byte{2}, 32))
	value, err := cipherA.encrypt("secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipherB.decrypt(value); err == nil {
		t.Fatal("expected decryption with wrong key to fail")
	}
}

func TestCredentialPlaintextWithMarkerIsStillEncrypted(t *testing.T) {
	cipher, _ := newCredentialCipher(bytes.Repeat([]byte{3}, 32))
	want := encryptedCredentialPrefix + "plain-token"
	stored, err := cipher.encrypt(want)
	if err != nil {
		t.Fatal(err)
	}
	if stored == want || !strings.HasPrefix(stored, encryptedCredentialPrefix) {
		t.Fatalf("credential was not encrypted: %q", stored)
	}
	got, err := cipher.decrypt(stored)
	if err != nil || got != want {
		t.Fatalf("decrypt = %q, %v", got, err)
	}
}

func TestStoreStartupMigratesLegacyCredentials(t *testing.T) {
	mini := miniredis.RunT(t)
	legacy, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "startup-migration:"})
	if err != nil {
		t.Fatal(err)
	}
	acc := &Account{Name: "legacy", AccountType: "grok", Enabled: true, ClientCookie: "legacy-on-disk"}
	if err := legacy.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	_ = legacy.Close()

	secure, err := New(Options{
		RedisAddr:               mini.Addr(),
		RedisPrefix:             "startup-migration:",
		CredentialEncryptionKey: bytes.Repeat([]byte{9}, 32),
	})
	if err != nil {
		t.Fatalf("secure reopen error = %v", err)
	}
	t.Cleanup(func() { _ = secure.Close() })
	raw, _ := mini.Get("startup-migration:accounts:id:1")
	if strings.Contains(raw, "legacy-on-disk") || !strings.Contains(raw, encryptedCredentialPrefix) {
		t.Fatalf("startup migration did not encrypt legacy account: %s", raw)
	}
	got, err := secure.GetAccount(context.Background(), acc.ID)
	if err != nil || got.ClientCookie != "legacy-on-disk" {
		t.Fatalf("migrated account = %#v, %v", got, err)
	}
}

func TestStoreStartupRejectsWrongCredentialKey(t *testing.T) {
	mini := miniredis.RunT(t)
	first, err := New(Options{
		RedisAddr: mini.Addr(), RedisPrefix: "wrong-key:",
		CredentialEncryptionKey: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CreateAccount(context.Background(), &Account{
		Name: "encrypted", AccountType: "grok", Enabled: true, ClientCookie: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if _, err := New(Options{
		RedisAddr: mini.Addr(), RedisPrefix: "wrong-key:",
		CredentialEncryptionKey: bytes.Repeat([]byte{2}, 32),
	}); err == nil {
		t.Fatal("expected startup with a different credential key to fail")
	}
	if _, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "wrong-key:"}); err == nil {
		t.Fatal("expected startup without a credential key to fail")
	}
}

func TestListAccountsReturnsCredentialDecryptionError(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{
		RedisAddr: mini.Addr(), RedisPrefix: "corrupt-list:",
		CredentialEncryptionKey: bytes.Repeat([]byte{4}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	mini.Set("corrupt-list:accounts:id:1", `{"id":1,"account_type":"grok","client_cookie":"enc:v1:not-valid"}`)
	mini.SAdd("corrupt-list:accounts:ids", "1")
	if _, err := s.ListAccounts(context.Background()); err == nil {
		t.Fatal("expected corrupted encrypted credential to be reported")
	}
}
