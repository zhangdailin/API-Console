package doubao

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
)

func sessionTestAccount() *store.Account {
	return &store.Account{AccountType: "doubao", Name: "administrator name", Enabled: true, Weight: 7,
		ClientCookie: `{"sessionid":"synthetic-old-session"}`, UserID: "synthetic-user",
		DoubaoDeviceID: "1000000000000000001", DoubaoWebID: "1000000000000000002", DoubaoTeaUUID: "1000000000000000002",
		DoubaoVerifiedAt: time.Now().Add(-time.Hour)}
}

func sessionTestStore(t *testing.T) *store.Store {
	t.Helper()
	mini := miniredis.RunT(t)
	s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "doubao-session:", CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSessionCookieRotationSurvivesNewClient(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "synthetic-rotated-session"
		if calls.Add(1) == 1 {
			want = "synthetic-old-session"
		}
		cookie, err := r.Cookie("sessionid")
		if err != nil || cookie.Value != want {
			t.Error("request used the wrong cookie generation")
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "synthetic-rotated-session", Path: "/", MaxAge: 3600})
		_, _ = io.WriteString(w, `{"data":{"user_id_str":"synthetic-user","name":"upstream name"}}`)
	}))
	defer srv.Close()
	cfg := &config.Config{DoubaoBaseURL: srv.URL}
	for range 2 {
		// Even an old caller snapshot must pick up the committed cookie.
		c := newClientWithStore(acc, cfg, s)
		info, err := c.AccountInfo(t.Context(), c.jar())
		if err != nil {
			t.Fatal(err)
		}
		info.apply(acc, time.Now())
		if err := s.UpdateAccount(t.Context(), acc); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil || decodeCookies(got.ClientCookie)["sessionid"] != "synthetic-rotated-session" || got.Name != "administrator name" || got.Weight != 7 {
		t.Fatal("rotation was not saved narrowly")
	}
}

func TestSessionUnknownAndTransientResponsesDoNotRequireLogin(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"csrf", 200, `{"data":{"error_code":2035}}`}, {"unknown", 200, `{}`},
		{"edge", 403, `<html>edge policy denied</html>`}, {"temporary", 503, `{"message":"login service temporarily unavailable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := NewFromAccount(sessionTestAccount(), &config.Config{DoubaoBaseURL: srv.URL})
			_, err := c.AccountInfo(t.Context(), c.jar())
			if err == nil || errors.Is(err, errSessionRejected) || accountpolicy.Classify(c.acc, err, "").NeedsLogin {
				t.Fatalf("non-auth failure became an expired session: %v", err)
			}
		})
	}
	if !accountpolicy.Classify(sessionTestAccount(), statusError(401, nil), "").NeedsLogin {
		t.Fatal("explicit authentication rejection lost its meaning")
	}
}

func TestSessionLateFailureCannotPoisonNewLogin(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := s.GetAccount(t.Context(), acc.ID)
		if err != nil {
			t.Error(err)
			return
		}
		current.ClientCookie = `{"sessionid":"synthetic-new-qr-session"}`
		current.AuthStatus, current.StatusCode = store.AccountAuthStatusActive, ""
		if err := s.UpdateAccount(t.Context(), current); err != nil {
			t.Error(err)
		}
		w.WriteHeader(401)
	}))
	defer srv.Close()
	maintain(t.Context(), provider.Env{Config: &config.Config{DoubaoBaseURL: srv.URL}, Store: s}, acc)
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil || !store.AccountAuthActive(got) || got.StatusCode != "" || decodeCookies(got.ClientCookie)["sessionid"] != "synthetic-new-qr-session" {
		t.Fatal("old failure poisoned the new session")
	}
}

func TestSessionLateSuccessCannotUndoAdminDisable(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current, err := s.GetAccount(t.Context(), acc.ID)
		if err != nil {
			t.Error(err)
			return
		}
		current.Enabled, current.Weight, current.Name = false, 11, "new admin name"
		if err := s.UpdateAccount(t.Context(), current); err != nil {
			t.Error(err)
		}
		_, _ = io.WriteString(w, `{"data":{"user_id_str":"synthetic-user","name":"upstream name"}}`)
	}))
	defer srv.Close()
	maintain(t.Context(), provider.Env{Config: &config.Config{DoubaoBaseURL: srv.URL}, Store: s}, acc)
	got, err := s.GetAccount(t.Context(), acc.ID)
	if err != nil || got.Enabled || got.Weight != 11 || got.Name != "new admin name" || time.Since(got.DoubaoVerifiedAt) > time.Minute {
		t.Fatal("health write overwrote administrator settings")
	}
}

func TestSessionHealthOnlyClearsCredentialVerdicts(t *testing.T) {
	for _, status := range []string{"401", "403", "429"} {
		t.Run(status, func(t *testing.T) {
			s := sessionTestStore(t)
			acc := sessionTestAccount()
			acc.AuthStatus, acc.StatusCode, acc.StatusMessage = store.AccountAuthStatusReauthRequired, status, "previous verdict"
			if err := s.CreateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"data":{"user_id_str":"synthetic-user"}}`)
			}))
			defer srv.Close()
			maintain(t.Context(), provider.Env{Config: &config.Config{DoubaoBaseURL: srv.URL}, Store: s}, acc)
			got, err := s.GetAccount(t.Context(), acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if status == "401" {
				if !store.AccountAuthActive(got) || got.StatusCode != "" {
					t.Fatal("healthy session retained its obsolete authentication block")
				}
			} else if got.StatusCode != status || store.AccountAuthActive(got) {
				t.Fatal("passport health cleared an unrelated restriction")
			}
		})
	}
}

func TestSessionConcurrentChecksUseLatestRotatedCookie(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		cookie, _ := r.Cookie("sessionid")
		if n > 1 && cookie.Value != "synthetic-rotated-session" {
			t.Error("waiting request retained an obsolete cookie")
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "synthetic-rotated-session", Path: "/"})
		_, _ = io.WriteString(w, `{"data":{"user_id_str":"synthetic-user"}}`)
	}))
	defer srv.Close()
	prototype := newClientWithStore(acc, &config.Config{DoubaoBaseURL: srv.URL}, s)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := prototype.AccountInfo(t.Context(), prototype.jar()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 8 {
		t.Fatal("unexpected request count")
	}
}

func TestSessionCookiesHonorDeletionAndRejectBroaderScope(t *testing.T) {
	jar := cookieJar{"sessionid": "synthetic", "expired": "synthetic"}
	request, _ := http.NewRequestWithContext(context.Background(), "GET", "https://www.doubao.com/", nil)
	response := &http.Response{Request: request, Header: http.Header{"Set-Cookie": []string{
		"sessionid=; Path=/; Max-Age=0", "expired=; Path=/; Expires=Thu, 01 Jan 1970 00:00:00 GMT",
		"foreign=private; Domain=foreign.invalid; Path=/", "narrow=private; Path=/passport/", "valid=synthetic; Domain=.doubao.com; Path=/; Secure",
	}}}
	jar.absorb(response)
	if len(jar) != 1 || jar["valid"] != "synthetic" {
		t.Fatal("cookie deletion or scope filtering failed")
	}
}

func TestSessionRejectsForeignTargetsAndUnsafeReplay(t *testing.T) {
	c := NewFromAccount(sessionTestAccount(), nil)
	if _, err := c.do(t.Context(), "GET", "https://foreign.invalid/", nil, c.jar(), false); !errors.Is(err, errSessionChanged) {
		t.Fatal("credentials could reach a foreign target")
	}
	for _, err := range []error{errSessionChanged, errSessionPersistence} {
		if accountpolicy.Retryable(err) || strings.Contains(err.Error(), "synthetic") {
			t.Fatal("session conflict could replay an accepted generation or expose credentials")
		}
	}
}

func TestSessionRejectsDisabledAndRotatedIdentityBeforeSending(t *testing.T) {
	for _, mode := range []string{"disabled", "identity", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			s := sessionTestStore(t)
			acc := sessionTestAccount()
			if err := s.CreateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer srv.Close()
			c := newClientWithStore(acc, &config.Config{DoubaoBaseURL: srv.URL}, s)
			switch mode {
			case "disabled":
				acc.Enabled = false
			case "identity":
				acc.DoubaoDeviceID = "new-device"
			case "rejected":
				acc.AuthStatus = store.AccountAuthStatusReauthRequired
			}
			if err := s.UpdateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			if _, err := c.do(t.Context(), "POST", srv.URL+"/chat/completion", nil, c.jar(), false); err == nil || calls.Load() != 0 {
				t.Fatal("ineligible session was sent upstream")
			}
		})
	}
}

func TestSessionNormalRequestsCommitRotationsAcrossHops(t *testing.T) {
	s := sessionTestStore(t)
	acc := sessionTestAccount()
	if err := s.CreateAccount(t.Context(), acc); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		cookie, _ := r.Cookie("sessionid")
		want := "synthetic-old-session"
		if n > 1 {
			want = "synthetic-rotated"
		}
		if cookie == nil || cookie.Value != want {
			t.Error("next hop missed rotation")
		}
		http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "synthetic-rotated", Path: "/"})
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	c := newClientWithStore(acc, &config.Config{DoubaoBaseURL: srv.URL}, s).scopedClient()
	jar := c.jar()
	for _, path := range []string{"/im/conversation/create", "/chat/completion"} {
		reply, err := c.do(t.Context(), "POST", srv.URL+path, nil, jar, false)
		if err != nil {
			t.Fatal(err)
		}
		_ = reply.Body.Close()
	}
	if calls.Load() != 2 {
		t.Fatal("unexpected replay")
	}
}

func TestSessionRedirectDoesNotForwardCookies(t *testing.T) {
	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1) }))
	defer foreign.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, foreign.URL, http.StatusFound) }))
	defer srv.Close()
	c := NewFromAccount(sessionTestAccount(), &config.Config{DoubaoBaseURL: srv.URL})
	reply, err := c.do(t.Context(), "GET", srv.URL+"/", nil, c.jar(), false)
	if err != nil {
		t.Fatal(err)
	}
	_ = reply.Body.Close()
	if reply.StatusCode != http.StatusFound || foreignCalls.Load() != 0 {
		t.Fatal("redirect forwarded credentials")
	}
}
