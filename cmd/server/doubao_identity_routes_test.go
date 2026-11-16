package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestDoubaoDeviceIdentityRouteRequiresAdminSession(t *testing.T) {
	cfg := &config.Config{AdminUser: "admin", AdminPass: "synthetic-password", AdminToken: "synthetic-admin-token", AdminPath: "/admin"}
	mux, s, _ := newRouteMux(t, "doubao-identity-routes:", cfg)
	observed := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	acc := &store.Account{
		AccountType: "doubao", Name: "synthetic identity route", ClientCookie: `{"sessionid":"synthetic-route-cookie"}`,
		DoubaoDeviceID: "1234567890123456789", DoubaoWebID: "2234567890123456789", DoubaoTeaUUID: "2234567890123456789",
		Enabled: false, StatusCode: "429", StatusMessage: "status=403: 710022004: human verification required",
		AuthStatus: store.AccountAuthStatusReauthRequired, LastAttempt: observed, QuotaResetAt: observed.Add(time.Hour),
		DoubaoVerifiedAt: observed, VerifiedAt: observed, UsageTotal: 17, RequestCount: 7,
	}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc))
	before, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err)
	path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10) + "/device-identity"
	call := func(method, authorization string, session *http.Cookie, adminToken string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Host = "localhost"
		req.Header.Set("Origin", "http://localhost")
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		if session != nil {
			req.AddCookie(session)
		}
		if adminToken != "" {
			req.Header.Set("X-Admin-Token", adminToken)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	const managedKey = "sk-synthetic-identity-managed-key"
	digest := sha256.Sum256([]byte(managedKey))
	testutil.NoError(t, s.CreateApiKey(t.Context(), &store.ApiKey{
		Name: "identity route managed key", KeyHash: hex.EncodeToString(digest[:]), KeyPrefix: "sk-", KeySuffix: "-key", Enabled: true,
	}))
	for _, authorization := range []string{"", "Bearer " + managedKey} {
		rec := call(http.MethodPost, authorization, nil, "")
		testutil.Equal(t, rec.Code, http.StatusUnauthorized)
		after, err := s.GetAccount(t.Context(), acc.ID)
		testutil.NoError(t, err)
		testutil.True(t, reflect.DeepEqual(after, before), "non-admin request changed the account")
	}

	// Obtain a real admin session through the registered login route rather than
	// injecting a session backend or treating an inference key as an admin.
	login := httptest.NewRecorder()
	mux.ServeHTTP(login, httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"username":"admin","password":"synthetic-password"}`)))
	testutil.Equal(t, login.Code, http.StatusOK)
	var session *http.Cookie
	for _, cookie := range login.Result().Cookies() {
		if cookie.Name == "session_token" {
			session = cookie
		}
	}
	testutil.True(t, session != nil, "login did not issue an admin session")

	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		rec := call(method, "", session, "")
		testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
		after, err := s.GetAccount(t.Context(), acc.ID)
		testutil.NoError(t, err)
		testutil.True(t, reflect.DeepEqual(after, before), "unsupported method changed the account through account routing")
	}

	for _, adminMode := range []string{"session", "admin-token"} {
		t.Run(adminMode, func(t *testing.T) {
			previous, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			var rec *httptest.ResponseRecorder
			if adminMode == "session" {
				rec = call(http.MethodPost, "", session, "")
			} else {
				rec = call(http.MethodPost, "", nil, cfg.AdminToken)
			}
			testutil.Equal(t, rec.Code, http.StatusOK)
			testutil.Equal(t, rec.Header().Get("Cache-Control"), "no-store")
			var out map[string]any
			testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
			testutil.Equal(t, len(out), 2)
			testutil.EqualAny(t, out["status"], "updated")
			testutil.EqualAny(t, out["account_id"], float64(acc.ID))
			after, err := s.GetAccount(t.Context(), acc.ID)
			testutil.NoError(t, err)
			testutil.True(t, after.DoubaoDeviceID != previous.DoubaoDeviceID && after.DoubaoWebID != previous.DoubaoWebID, "admin action did not rotate both identities")
			testutil.True(t, after.DoubaoWebID == after.DoubaoTeaUUID, "web identity and tea UUID diverged")
			expected := previous.Clone()
			expected.DoubaoDeviceID, expected.DoubaoWebID, expected.DoubaoTeaUUID = after.DoubaoDeviceID, after.DoubaoWebID, after.DoubaoTeaUUID
			expected.UpdatedAt = after.UpdatedAt
			testutil.True(t, reflect.DeepEqual(after, expected), "admin action changed park, enablement, credential, verification, or usage state")
			for _, field := range []string{"client_cookie", "sessionid", "doubao_device_id", "doubao_web_id", "doubao_tea_uuid"} {
				testutil.True(t, !strings.Contains(rec.Body.String(), field), "route response exposed a private identity field")
			}
			for _, account := range []*store.Account{previous, after} {
				for _, private := range []string{account.ClientCookie, account.DoubaoDeviceID, account.DoubaoWebID, account.DoubaoTeaUUID} {
					testutil.True(t, !strings.Contains(rec.Body.String(), private), "route response exposed a private identity value")
				}
			}
		})
	}
}
