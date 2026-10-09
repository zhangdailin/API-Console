package store

import (
	"bytes"
	"context"
	"orchids-api/internal/testutil"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

func TestAccountCredentialsEncryptedAndPlaintextRejected(t *testing.T) {
	mini := miniredis.RunT(t)
	key := bytes.Repeat([]byte{0x2a}, 32)
	s, err := New(Options{
		RedisAddr:               mini.Addr(),
		RedisPrefix:             "cipher-test:",
		CredentialEncryptionKey: key,
	})
	testutil.NoError(t, err, "New() error = %v")
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
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount() error = %v")
	raw, err := mini.Get("cipher-test:accounts:id:1")
	testutil.NoError(t, err, "read raw account: %v")
	for _, secret := range []string{
		"sso-secret", "access-secret", "refresh-secret", "wb-access-secret", "wb-refresh-secret",
		"qoder-access-secret", "qoder-refresh-secret", "qoder-runtime-secret", "qoder-key-secret", "qoder-job-secret",
	} {
		testutil.MustNotContain(t, raw, secret)
	}
	testutil.MustContain(t, raw, encryptedCredentialPrefix)
	got, err := s.GetAccount(ctx, acc.ID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, got.ClientCookie, acc.ClientCookie)
	testutil.Equal(t, got.OAuthAccessToken, acc.OAuthAccessToken)
	testutil.Equal(t, got.OAuthRefreshToken, acc.OAuthRefreshToken)
	if got.WorkBuddyRefreshToken != acc.WorkBuddyRefreshToken || got.QoderRefreshToken != acc.QoderRefreshToken ||
		got.QoderRuntimeKey != acc.QoderRuntimeKey {
		t.Fatalf("decrypted provider credentials mismatch: %#v", got)
	}

	legacy := `{"id":2,"name":"legacy","account_type":"grok","enabled":true,"client_cookie":"legacy-secret"}`
	mini.Set("cipher-test:accounts:id:2", legacy)
	mini.SAdd("cipher-test:accounts:ids", "2")
	_, err = s.GetAccount(ctx, 2)
	testutil.Error(t, err)
	unchanged, _ := mini.Get("cipher-test:accounts:id:2")
	testutil.Equal(t, unchanged, legacy)

}

// Retired account metadata is intentionally dropped even when an old Redis row
// still carries it; only the live provider credentials survive a rewrite.
func TestRetiredAccountFieldsAreDroppedOnRewrite(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "retired-fields:"})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	mini.Set("retired-fields:accounts:id:1", `{"id":1,"name":"legacy","account_type":"workbuddy","enabled":true,"nsfw_enabled":true,"device_id":"old-device","request_id":"old-request","project_id":"old-project","upstream_mode":"old-mode","workbuddy_refresh_token":"live-refresh"}`)
	mini.SAdd("retired-fields:accounts:ids", "1")
	acc, err := s.GetAccount(ctx, 1)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, acc.WorkBuddyRefreshToken, "live-refresh")
	acc.Name = "rewritten"
	testutil.NoError(t, s.UpdateAccount(ctx, acc))
	raw, err := mini.Get("retired-fields:accounts:id:1")
	testutil.NoError(t, err)
	for _, field := range []string{"nsfw_enabled", "device_id", "request_id", "project_id", "upstream_mode"} {
		testutil.CheckNotContain(t, raw, `"`+field+`"`)
	}
	testutil.CheckContain(t, raw, `"workbuddy_refresh_token":"live-refresh"`)
}

func TestEncryptedAccountRejectsWrongKey(t *testing.T) {
	cipherA, _ := newCredentialCipher(bytes.Repeat([]byte{1}, 32))
	cipherB, _ := newCredentialCipher(bytes.Repeat([]byte{2}, 32))
	value, err := cipherA.encrypt("secret")
	testutil.NoError(t, err)
	_, err = cipherB.decrypt(value)
	testutil.Error(t, err)
}

func TestCredentialPlaintextWithMarkerIsStillEncrypted(t *testing.T) {
	cipher, _ := newCredentialCipher(bytes.Repeat([]byte{3}, 32))
	want := encryptedCredentialPrefix + "plain-token"
	stored, err := cipher.encrypt(want)
	testutil.NoError(t, err)
	testutil.Falsef(t, stored == want || !strings.HasPrefix(stored, encryptedCredentialPrefix), "credential was not encrypted: %q", stored)
	got, err := cipher.decrypt(stored)
	testutil.Equal(t, err, nil)
	testutil.Equal(t, got, want)
}

func TestStoreStartupRejectsPlaintextWithoutMigration(t *testing.T) {
	mini := miniredis.RunT(t)
	legacy, err := New(Options{RedisAddr: mini.Addr(), RedisPrefix: "startup-migration:"})
	testutil.NoError(t, err)
	acc := &Account{Name: "legacy", AccountType: "grok", Enabled: true, ClientCookie: "legacy-on-disk"}
	testutil.NoError(t, legacy.CreateAccount(context.Background(), acc))
	_ = legacy.Close()

	secure, err := New(Options{
		RedisAddr:               mini.Addr(),
		RedisPrefix:             "startup-migration:",
		CredentialEncryptionKey: bytes.Repeat([]byte{9}, 32),
	})
	testutil.Error(t, err)
	testutil.Equal(t, secure, (*Store)(nil))
	raw, _ := mini.Get("startup-migration:accounts:id:1")
	testutil.MustContain(t, raw, "legacy-on-disk")
	testutil.MustNotContain(t, raw, encryptedCredentialPrefix)

}

func TestStoreStartupRejectsWrongCredentialKey(t *testing.T) {
	mini := miniredis.RunT(t)
	first, err := New(Options{
		RedisAddr: mini.Addr(), RedisPrefix: "wrong-key:",
		CredentialEncryptionKey: bytes.Repeat([]byte{1}, 32),
	})
	testutil.NoError(t, err)
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
	_, err = New(Options{RedisAddr: mini.Addr(), RedisPrefix: "wrong-key:"})
	testutil.Error(t, err)
}

func TestListAccountsReturnsCredentialDecryptionError(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := New(Options{
		RedisAddr: mini.Addr(), RedisPrefix: "corrupt-list:",
		CredentialEncryptionKey: bytes.Repeat([]byte{4}, 32),
	})
	testutil.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	mini.Set("corrupt-list:accounts:id:1", `{"id":1,"account_type":"grok","client_cookie":"enc:v1:not-valid"}`)
	mini.SAdd("corrupt-list:accounts:ids", "1")
	_, err = s.ListAccounts(context.Background())
	testutil.Error(t, err)
}
