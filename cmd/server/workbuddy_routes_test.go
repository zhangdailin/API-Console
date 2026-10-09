package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

// workbuddyAuthStub is the local stand-in for the WorkBuddy authorization
// service. It answers the start request with a state and keeps every poll
// pending, so this test never reaches the real deployment.
func workbuddyAuthStub(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			testutil.CheckEqual(t, r.URL.Query().Get("platform"), "workbuddy-ai")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"state-routes","authUrl":"https://www.workbuddy.ai/login?platform=workbuddy-ai&state=state-routes"}}`))
		case "/v2/plugin/auth/token":
			// Pending forever: the transaction is cancelled below, and no
			// credential is ever exchanged with this stub.
			_, _ = w.Write([]byte(`{"code":11217,"msg":"11217:login ing..."}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestRegisterRoutes_WorkBuddyEndpoints proves the WorkBuddy inference channel
// and the official browser login are reachable through the real route table and
// stay behind their respective auth layers.
func TestRegisterRoutes_WorkBuddyEndpoints(t *testing.T) {
	auth := workbuddyAuthStub(t)
	defer auth.Close()

	// Every WorkBuddy upstream call is redirected to the stub, so the default
	// suite stays offline: without this the login start would hit the real
	// authorization service.
	cfg := &config.Config{
		AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin",
		WorkBuddyBaseURL:  auth.URL,
		AnonymousAllowIPs: []string{"192.0.2.1"},
	}
	mux, _, _ := newRouteMux(t, "routes:", cfg)

	// The login endpoints must not be usable without an admin session.
	unauth := httptest.NewRecorder()
	mux.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/workbuddy/login", strings.NewReader("{}")))
	testutil.Falsef(t, unauth.Code != http.StatusUnauthorized && unauth.Code != http.StatusForbidden, "unauthenticated login status = %d, want 401/403", unauth.Code)

	// Authorized requests reach the handler, which asks the (local) upstream for
	// a fresh authorization transaction. localhost is used so the same-origin
	// HTTPS guard accepts the plain-HTTP test request.
	req := httptest.NewRequest(http.MethodPost, "/api/workbuddy/login", strings.NewReader("{}"))
	req.Header.Set("X-Admin-Token", "admintoken")
	req.Header.Set("Origin", "http://localhost")
	req.Header.Set("Content-Type", "application/json")
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)
	var started struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &started)
	testutil.CheckNoError(t, err)
	testutil.Falsef(t, started.ID == "" || started.Status != "pending", "login response = %+v", started)
	testutil.Falsef(t, !strings.HasPrefix(started.VerificationURIComplete, "https://www.workbuddy.ai/login?"), "login response is not the official login page: %q", started.VerificationURIComplete)

	// Poll the transaction, then cancel it so the test leaves no pending login.
	pollReq := httptest.NewRequest(http.MethodGet, "/api/workbuddy/login/"+started.ID, nil)
	pollReq.Header.Set("X-Admin-Token", "admintoken")
	pollRec := httptest.NewRecorder()
	mux.ServeHTTP(pollRec, pollReq)
	testutil.Equal(t, pollRec.Code, http.StatusOK)
	cancelReq := httptest.NewRequest(http.MethodDelete, "/api/workbuddy/login/"+started.ID, nil)
	cancelReq.Header.Set("X-Admin-Token", "admintoken")
	cancelRec := httptest.NewRecorder()
	mux.ServeHTTP(cancelRec, cancelReq)
	testutil.Equal(t, cancelRec.Code, http.StatusNoContent)

	// An unregistered path is what a missing route looks like on this mux; every
	// path below must answer differently, which is what proves it is routed.
	unknown := httptest.NewRecorder()
	mux.ServeHTTP(unknown, httptest.NewRequest(http.MethodPost, "/definitely-not-registered", strings.NewReader("{}")))
	unknownBody := unknown.Body.String()
	testutil.MustContain(t, unknownBody, "404 page not found")

	// Both provider bases reject untrusted clients; allowlisted controls must
	// reach the API handler so auth cannot conceal a registration regression.
	for _, target := range []string{
		"/workbuddy/v1/messages",
		"/workbuddy/v1/chat/completions",
		"/workbuddy/v1/models",
		"/workbuddy/v1/responses",
		"/workbuddy/v1/responses/",
		"/workbuddy/v1/responses/compact",
		"/cline/v1/responses",
		"/qoder/v1/responses",
		// The unversioned provider bases share the registered API capabilities.
		"/workbuddy/chat/completions",
		"/workbuddy/messages",
		"/grok/messages/count_tokens",
		"/workbuddy/responses",
	} {
		method := http.MethodPost
		if strings.HasSuffix(target, "/models") {
			method = http.MethodGet
		}
		channelReq := httptest.NewRequest(method, target, strings.NewReader(`{"model":"hy3","messages":[]}`))
		channelReq.RemoteAddr = "198.51.100.1:12345"
		channelRec := httptest.NewRecorder()
		mux.ServeHTTP(channelRec, channelReq)
		testutil.Equal(t, channelRec.Code, http.StatusUnauthorized)
		allowedReq := httptest.NewRequest(method, target, strings.NewReader(`{"model":"hy3","messages":[]}`))
		allowedReq.RemoteAddr = "192.0.2.1:12345"
		allowedRec := httptest.NewRecorder()
		mux.ServeHTTP(allowedRec, allowedReq)
		testutil.Falsef(t, allowedRec.Code == http.StatusUnauthorized || allowedRec.Code == http.StatusForbidden, "%s did not get through inference auth: %d %s", target, allowedRec.Code, allowedRec.Body.String())
		testutil.Falsef(t, allowedRec.Code == http.StatusMethodNotAllowed || (allowedRec.Code >= 300 && allowedRec.Code < 400), "%s %s rejected its registered method or redirected: %d %s", method, target, allowedRec.Code, allowedRec.Body.String())
		testutil.Falsef(t, !json.Valid(allowedRec.Body.Bytes()), "%s returned no API JSON envelope: %d %s", target, allowedRec.Code, allowedRec.Body.String())
		testutil.Falsef(t, allowedRec.Body.String() == unknownBody || strings.Contains(allowedRec.Body.String(), "404 page not found"), "%s answered like an unregistered route: %d %s", target, allowedRec.Code, allowedRec.Body.String())
	}
	// The same allowlisted request to a missing /v1 path must now expose its
	// real 404, instead of the authentication response used above.
	missingReq := httptest.NewRequest(http.MethodPost, "/v1/definitely-not-registered", strings.NewReader("{}"))
	missingReq.RemoteAddr = "192.0.2.1:12345"
	missingRec := httptest.NewRecorder()
	mux.ServeHTTP(missingRec, missingReq)
	testutil.Falsef(t, missingRec.Code != http.StatusNotFound || missingRec.Body.String() != unknownBody, "missing allowlisted /v1 route = %d %s, want plain 404", missingRec.Code, missingRec.Body.String())
}
