package warp

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func TestOfficialLoginPendingExchangeBuild(t *testing.T) {
	pending := true
	token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"test@example.invalid","sub":"test-user"}`)) + ".signature"
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/api/v1/oauth/device/auth":
			if r.PostForm.Get("client_id") != "warp-agent-cli" {
				t.Error("wrong device client")
			}
			_, _ = w.Write([]byte(`{"device_code":"synthetic-device","user_code":"TEST","verification_uri":"https://app.warp.dev/device","expires_in":600,"interval":2}`))
		case "/api/v1/oauth/token":
			if r.PostForm.Get("device_code") != "synthetic-device" || r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Error("wrong device exchange")
			}
			if pending {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"synthetic-custom"}`))
		case "/v1/accounts:signInWithCustomToken":
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.PostForm.Get("token") != "synthetic-custom" || r.PostForm.Get("returnSecureToken") != "true" || r.URL.Query().Get("key") != "synthetic-project-config" {
				t.Error("wrong Firebase custom token form")
			}
			_, _ = w.Write([]byte(`{"idToken":"` + token + `","refreshToken":"synthetic-durable","expiresIn":"3600"}`))
		case "/graphql/v2":
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("catalog unauthenticated")
			}
			_, _ = w.Write([]byte(`{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"model-a"}]}}}]}}}}`))
		default:
			t.Error("unexpected login path")
			w.WriteHeader(400)
		}
	}))
	defer s.Close()
	cfg := testConfig(s.URL)
	port := loginPort()
	ch, err := port.Start(context.Background(), cfg)
	if err != nil || ch.UserCode != "TEST" {
		t.Fatalf("start failed %v", err)
	}
	if _, err = port.Exchange(context.Background(), cfg, ch); !errors.Is(err, ErrAuthPending) {
		t.Fatal("pending not classified")
	}
	pending = false
	grant, err := port.Exchange(context.Background(), cfg, ch)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := port.Build(context.Background(), cfg, "login-id", ch, grant)
	if err != nil {
		t.Fatal(err)
	}
	if acc.WarpRefreshToken != "synthetic-durable" || acc.Email != "test@example.invalid" || acc.UserID != "test-user" || len(acc.WarpModelIDs) != 1 {
		t.Fatal("wrong login account")
	}
	adminPort().Project(acc)
	if acc.WarpAccessToken != "" || acc.WarpRefreshToken != "" {
		t.Fatal("login credential leaked")
	}
}
func TestMissingFirebaseKeyMakesNoNetwork(t *testing.T) {
	t.Setenv("ORCHIDS_WARP_FIREBASE_API_KEY", "")
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer s.Close()
	if _, err := loginPort().Start(context.Background(), &config.Config{WarpBaseURL: s.URL}); err == nil || calls != 0 {
		t.Fatal("missing configuration should fail locally")
	}
}

func TestRefreshRotatesAtomicallyAndEncryptedPersistence(t *testing.T) {
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "warp-test:", CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acc := testAccount()
	acc.WarpExpiresAt = time.Now().Add(-time.Hour)
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		if r.URL.Path != "/v1/token" {
			t.Error("refresh reached wrong endpoint")
		}
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("refresh_token") != "synthetic-refresh" {
			t.Error("wrong refresh form")
		}
		_, _ = w.Write([]byte(`{"id_token":"synthetic-renewed-access","refresh_token":"synthetic-rotated-refresh","expires_in":"3600"}`))
	}))
	defer server.Close()
	c1 := newClientWithStore(acc, testConfig(server.URL), s)
	c2 := newClientWithStore(acc, testConfig(server.URL), s)
	var wg sync.WaitGroup
	for _, c := range []*Client{c1, c2} {
		wg.Add(1)
		go func(c *Client) {
			defer wg.Done()
			token, err := c.accessToken(context.Background())
			if err != nil || token != "synthetic-renewed-access" {
				t.Error("rotation failed")
			}
		}(c)
	}
	wg.Wait()
	if refreshes.Load() != 1 {
		t.Fatal("rotating grant spent more than once")
	}
	stored, err := s.GetAccount(context.Background(), acc.ID)
	if err != nil || stored.WarpRefreshToken != "synthetic-rotated-refresh" || stored.WarpAccessToken != "synthetic-renewed-access" {
		t.Fatal("rotation not durable")
	}
	for _, key := range mini.Keys() {
		if mini.Type(key) != "string" {
			continue
		}
		raw, _ := mini.Get(key)
		if strings.Contains(raw, "synthetic-rotated-refresh") || strings.Contains(raw, "synthetic-renewed-access") {
			t.Fatal("plaintext credential persisted")
		}
	}
}
func TestAdminOfficialLoginOnly(t *testing.T) {
	admin := adminPort()
	if admin.PrepareCreate(testAccount()) == nil || admin.PrepareImport(testAccount()) != "official_login_required" {
		t.Fatal("external credential creation allowed")
	}
	existing := testAccount()
	edit := &store.Account{Name: "updated"}
	if err := admin.PrepareUpdate(edit, existing); err != nil || edit.WarpRefreshToken != existing.WarpRefreshToken {
		t.Fatal("edit lost credential")
	}
	edit = &store.Account{WarpRefreshToken: "pasted"}
	if admin.PrepareUpdate(edit, existing) == nil {
		t.Fatal("credential edit allowed")
	}
	if admin.Login == nil || !admin.Login.BuildVerifies {
		t.Fatal("official login not registered")
	}
}

// A transient catalog/quota outage must not discard the credential that the
// Firebase exchange just proved. Maintenance retries the observations later.
func TestBuildSurvivesCatalogAndQuotaFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/graphql/v2" {
			_, _ = w.Write([]byte(`{"errors":[{"message":"synthetic upstream outage"}]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer s.Close()
	cfg := testConfig(s.URL)
	grant := Credentials{AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour), Email: "user@example.invalid", UserID: "uid"}
	acc, err := loginPort().Build(context.Background(), cfg, "login-id", provider.LoginChallenge{}, grant)
	if err != nil {
		t.Fatalf("transient catalog outage discarded the login: %v", err)
	}
	if acc.WarpRefreshToken != "synthetic-refresh" {
		t.Fatal("credential was not retained")
	}
	if len(acc.WarpModelIDs) != 0 {
		t.Fatal("failed catalog invented model IDs")
	}
}
