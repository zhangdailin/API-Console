package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"orchids-api/internal/auth"
	"orchids-api/internal/config"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestClineCheckSeparatesAccountRejectionFromAdminAuthentication(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			http.Error(w, `{"error":{"message":"failed to refresh token: invalid_grant"}}`, http.StatusBadRequest)
		case "/workos":
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	s, _ := newTestStore(t, "cline-admin-check:")
	acc := &store.Account{AccountType: "cline", Enabled: true, Weight: 1, ClineRefreshToken: "refresh"}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	cfg := &config.Config{
		AdminUser: "admin", AdminPass: "test-password",
		ClineAPIBaseURL: upstream.URL, ClineWorkOSTokenURL: upstream.URL + "/workos",
	}
	a := New(s, cfg.AdminUser, cfg.AdminPass, cfg)
	login := httptest.NewRecorder()
	a.HandleLogin(login, channelLoginRequest(t, http.MethodPost, "/api/login", `{"username":"admin","password":"test-password"}`))
	testutil.Equal(t, login.Code, http.StatusOK)
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "session_token" {
		t.Fatal("expected an admin session cookie")
	}
	t.Cleanup(func() { auth.InvalidateSessionToken(cookies[0].Value) })

	handler := middleware.SessionAuthDynamic(func() (string, string) { return cfg.AdminPass, "" }, a.HandleAccountByID)
	path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10)
	request := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	check := request(path+"/check", cookies[0])
	testutil.Equal(t, check.Code, http.StatusConflict)
	testutil.CheckContain(t, check.Body.String(), "new browser login is required")
	row := request(path, cookies[0])
	testutil.Equal(t, row.Code, http.StatusOK)
	testutil.CheckContain(t, row.Body.String(), `"status_code":"401"`)
	testutil.Equal(t, request(path+"/check", nil).Code, http.StatusUnauthorized)
}

func TestStaleReplicaUsesAuthoritativePassword(t *testing.T) {
	a, s, mini := setupConfigAPI(t)
	stale := a.ConfigSnapshot().Clone()
	auth.SetSessionBackend(auth.NewRedisSessionBackend(s.RedisClient(), s.RedisPrefix()))
	t.Cleanup(func() { auth.SetSessionBackend(nil); auth.ClearLocalSessions() })
	next := stale.Clone()
	next.AdminUser, next.AdminPass = "new-admin", "new-password"
	testutil.NoError(t, a.persistConfig(t.Context(), stale, next))
	// An unrelated save on the old replica must not resurrect its password.
	testutil.Error(t, a.persistConfig(t.Context(), stale, stale))
	handler := middleware.SessionAuthDynamic(func() (string, string) { return stale.AdminPass, stale.AdminToken }, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	for _, basic := range []bool{false, true} {
		for _, password := range []string{stale.AdminPass, next.AdminPass} {
			r := httptest.NewRequest("GET", "/api/config/list", nil)
			if basic {
				r.SetBasicAuth("ignored", password)
			} else {
				r.Header.Set("Authorization", "Bearer "+password)
			}
			w := httptest.NewRecorder()
			handler(w, r)
			want := 401
			if password == next.AdminPass {
				want = 204
			}
			testutil.Equal(t, w.Code, want)
		}
	}
	replica := New(s, stale.AdminUser, stale.AdminPass, stale)
	w := httptest.NewRecorder()
	replica.HandleLogin(w, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"new-admin","password":"new-password"}`)))
	testutil.Equal(t, w.Code, 200)
	// Shared configuration corruption and loss must never fall back to stale credentials.
	mini.Set(s.RedisPrefix()+"settings:config", "invalid-json")
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/config/list", nil)
	r.SetBasicAuth("admin", stale.AdminPass)
	handler(w, r)
	testutil.Equal(t, w.Code, 503)
	mini.Del(s.RedisPrefix() + "settings:config")
	w = httptest.NewRecorder()
	handler(w, r)
	testutil.Equal(t, w.Code, 503)
	mini.Close()
	w = httptest.NewRecorder()
	handler(w, r)
	testutil.Equal(t, w.Code, 503)
}

func TestCookieConfigMutationRequiresSameOriginJSON(t *testing.T) {
	a, _, _ := setupConfigAPI(t)
	auth.SetSessionBackend(nil)
	t.Cleanup(auth.ClearLocalSessions)
	token, err := auth.GenerateSessionTokenForPassword("")
	testutil.NoError(t, err)
	h := middleware.SessionAuthDynamic(func() (string, string) { return "pass", "static" }, a.HandleConfigSave)
	for _, tc := range []struct {
		origin, media string
		want          int
	}{
		{"https://sibling.example.com", "application/json", 403},
		{"", "application/json", 403},
		{"http://admin.example.com", "application/json", 403},
		{"https://admin.example.com", "text/plain", 415},
		{"https://admin.example.com", "application/json", 200},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "https://admin.example.com/api/config/save", strings.NewReader(`{}`))
		r.AddCookie(&http.Cookie{Name: "session_token", Value: token})
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Content-Type", tc.media)
		h(w, r)
		testutil.Equal(t, w.Code, tc.want)
	}
	// A CLI uses explicit credentials and needs no browser Origin header.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "https://admin.example.com/api/config/save", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer static")
	h(w, r)
	testutil.Equal(t, w.Code, 200)
}

type unreadLoginBody struct{ t *testing.T }

func (b unreadLoginBody) Read([]byte) (int, error) {
	b.t.Fatal("blocked login read its body")
	return 0, context.Canceled
}
func (unreadLoginBody) Close() error { return nil }

func TestLoginParsingBoundariesAndPreAdmission(t *testing.T) {
	for _, tc := range []struct {
		body, media string
		want        int
	}{
		{`{"username":"` + strings.Repeat("x", 9<<10) + `"}`, "application/json", 413},
		{`{"username":"` + strings.Repeat("x", 257) + `"}`, "application/json", 400},
		{`{"username":"admin","password":"initial-secret"} {}`, "application/json", 400},
		{`{`, "application/json", 400},
		{`{}`, "text/plain", 415},
	} {
		a, _, _ := setupConfigAPI(t)
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", tc.media)
		a.HandleLogin(w, r)
		testutil.Equal(t, w.Code, tc.want)
	}
	a, _, _ := setupConfigAPI(t)
	for range 5 {
		w := httptest.NewRecorder()
		a.HandleLogin(w, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"password":"wrong"}`)))
		testutil.Equal(t, w.Code, 401)
	}
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.Body = unreadLoginBody{t}
	w := httptest.NewRecorder()
	a.HandleLogin(w, r)
	testutil.Equal(t, w.Code, 429)
	for i := 0; i < cap(loginAdmission); i++ {
		loginAdmission <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(loginAdmission); i++ {
			<-loginAdmission
		}
	})
	w = httptest.NewRecorder()
	a.HandleLogin(w, r)
	testutil.Equal(t, w.Code, 503)
}
