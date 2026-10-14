package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/auth"
	"orchids-api/internal/middleware"
	"orchids-api/internal/testutil"
)

func TestConfigBlankPasswordPreservesSecrets(t *testing.T) {
	for _, patch := range []string{`{}`, `{"admin_password":""}`, `{"admin_password":"  "}`, `{"admin_password":null}`, `{"admin_pass":""}`} {
		t.Run(patch, func(t *testing.T) {
			a, s, _ := setupConfigAPI(t)
			current := a.ConfigSnapshot()
			out := httptest.NewRecorder()
			a.HandleConfigSave(out, httptest.NewRequest("POST", "/api/config/save", strings.NewReader(patch)))
			testutil.Equal(t, a.ConfigSnapshot().AdminPass, current.AdminPass)
			testutil.Equal(t, a.ConfigSnapshot().AdminToken, current.AdminToken)
			raw, err := s.GetSetting(context.Background(), "config")
			testutil.NoError(t, err)
			var saved map[string]any
			testutil.NoError(t, json.Unmarshal([]byte(raw), &saved))
			testutil.Equal(t, saved["admin_token"], any(current.AdminToken))
		})
	}
}

func TestPasswordRotationRevokesSessionsAcrossReplicas(t *testing.T) {
	a, s, _ := setupConfigAPI(t)
	backend := auth.NewRedisSessionBackend(s.RedisClient(), s.RedisPrefix())
	auth.SetSessionBackend(backend)
	t.Cleanup(func() { auth.SetSessionBackend(nil); auth.ClearLocalSessions() })
	ctx := context.Background()
	testutil.NoError(t, a.persistConfig(ctx, a.ConfigSnapshot(), a.ConfigSnapshot()))
	first, err := auth.GenerateSessionTokenForPassword(a.ConfigSnapshot().AdminPass)
	testutil.NoError(t, err)
	second, err := auth.GenerateSessionTokenForPassword(a.ConfigSnapshot().AdminPass)
	testutil.NoError(t, err)
	next := a.ConfigSnapshot().Clone()
	oldPassword := next.AdminPass
	next.AdminPass = "rotated-test-password"
	testutil.NoError(t, a.persistConfig(ctx, a.ConfigSnapshot(), next))
	// A second backend represents a different replica with its own memory cache.
	replica := auth.NewRedisSessionBackend(s.RedisClient(), s.RedisPrefix())
	for _, token := range []string{first, second} {
		valid, err := replica.HasSession(ctx, token)
		testutil.NoError(t, err)
		testutil.False(t, valid)
		testutil.False(t, auth.ValidateSessionToken(token))
	}
	_, err = auth.GenerateSessionTokenForPassword(oldPassword)
	testutil.Error(t, err)
	fresh, err := auth.GenerateSessionTokenForPassword(next.AdminPass)
	testutil.NoError(t, err)
	testutil.True(t, auth.ValidateSessionToken(fresh))
	testutil.NoError(t, a.persistConfig(ctx, next, next))
	testutil.True(t, auth.ValidateSessionToken(fresh), "unrelated saves must preserve sessions")
}

func TestAdminPasswordEntrancesShareFailureBudget(t *testing.T) {
	a, _, _ := setupConfigAPI(t)
	credentials := func() (string, string) { return a.ConfigSnapshot().AdminPass, a.ConfigSnapshot().AdminToken }
	protected := middleware.SessionAuthDynamic(credentials, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }, a.AdminAuthenticationLimiter())
	for i := range 5 {
		out := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/api/config/list", nil)
		if i%2 == 0 {
			req.SetBasicAuth("admin", "wrong")
		} else {
			req.Header.Set("X-Admin-Token", "wrong")
		}
		protected(out, req)
		testutil.Equal(t, out.Code, 401)
	}
	login := httptest.NewRecorder()
	a.HandleLogin(login, httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"username":"admin","password":"initial-secret"}`)))
	testutil.Equal(t, login.Code, 429)
	req := httptest.NewRequest("GET", "/api/config/list", nil)
	req.Header.Set("X-Admin-Token", a.ConfigSnapshot().AdminToken)
	out := httptest.NewRecorder()
	protected(out, req)
	testutil.Equal(t, out.Code, 204)
}
