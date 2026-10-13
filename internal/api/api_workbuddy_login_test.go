package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/workbuddy"
)

// workbuddyAuthServer stubs the WorkBuddy authorization endpoints the login
// flow uses. It records nothing secret and never echoes credentials.
func workbuddyAuthServer(t *testing.T, tokenStatus []string, catalog string) *httptest.Server {
	t.Helper()
	var polls int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			testutil.CheckEqual(t, r.Method, http.MethodPost)
			testutil.CheckEqual(t, r.URL.Query().Get("platform"), "workbuddy-ai")
			testutil.CheckNotEqual(t, r.Header.Get("X-No-Authorization"), "")
			_, _ = w.Write([]byte(`{"code":0,"msg":"OK","data":{"state":"state-123","authUrl":"https://www.workbuddy.ai/login?platform=workbuddy-ai&state=state-123"}}`))
		case "/v2/plugin/auth/token":
			testutil.CheckEqual(t, r.URL.Query().Get("state"), "state-123")
			index := polls
			polls++
			if index >= len(tokenStatus) {
				index = len(tokenStatus) - 1
			}
			_, _ = w.Write([]byte(tokenStatus[index]))
		case "/v2/plugin/login/account":
			_, _ = w.Write([]byte(`{"code":0,"message":"","data":{"uid":"uid-abc","nickname":"operator@example.com","email":"operator@example.com"}}`))
		case "/v3/config":
			_, _ = w.Write([]byte(catalog))
		default:
			http.NotFound(w, r)
		}
	}))
}

const workbuddyCatalogBody = `{"code":0,"data":{"models":[
  {"id":"hy3","name":"HY3","disabled":false},
  {"id":"default-model","name":"Default","disabled":false}
],"agents":[{"name":"cli","models":["default-model","hy3"]}]}}`

// testJWT builds an unsigned token whose payload carries the identity claims the
// client reads. The upstream verifies the signature; the gateway only needs a
// decodable payload to learn the account identity.
func testJWT(uid, email string) string {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	header := encode(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload := encode(map[string]any{
		"sub":   uid,
		"email": email,
		"iss":   "https://www.workbuddy.ai/auth/realms/copilot",
		"exp":   time.Now().Add(48 * time.Hour).Unix(),
	})
	return header + "." + payload + ".signature"
}

// stubWorkBuddyLoginClient points the WorkBuddy login flow at a stub upstream.
// Only the login is redirected: the API's own configuration is left alone, as
// it would be for an operator who never set a base URL.
func stubWorkBuddyLoginClient(t *testing.T, baseURL string) {
	t.Helper()
	registered := registeredLoginFlow(channel.WorkBuddy)
	testutil.True(t, registered != nil, "WorkBuddy must register a login flow")
	redirect := func(cfg *config.Config) *config.Config {
		configured := config.Config{}
		if cfg != nil {
			configured = *cfg
		}
		configured.WorkBuddyBaseURL = baseURL
		return &configured
	}
	stub := *registered
	stub.Start = func(ctx context.Context, cfg *config.Config) (provider.LoginChallenge, error) {
		return registered.Start(ctx, redirect(cfg))
	}
	stub.Exchange = func(ctx context.Context, cfg *config.Config, challenge provider.LoginChallenge) (provider.LoginGrant, error) {
		return registered.Exchange(ctx, redirect(cfg), challenge)
	}
	stub.Build = func(ctx context.Context, cfg *config.Config, loginID string, challenge provider.LoginChallenge, grant provider.LoginGrant) (*store.Account, error) {
		return registered.Build(ctx, redirect(cfg), loginID, challenge, grant)
	}

	resolveLoginFlowMu.Lock()
	previous := resolveLoginFlow
	resolveLoginFlow = func(id channel.ID) *provider.Login {
		if id == channel.WorkBuddy {
			return &stub
		}
		return previous(id)
	}
	resolveLoginFlowMu.Unlock()
	t.Cleanup(func() {
		resolveLoginFlowMu.Lock()
		resolveLoginFlow = previous
		resolveLoginFlowMu.Unlock()
	})
}

// decodeLoginError returns the machine-readable code the admin UI uses to
// explain a failed login attempt.
func decodeLoginError(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	err := json.Unmarshal([]byte(body), &payload)
	testutil.CheckNoError(t, err)
	testutil.NotEqual(t, payload.Error, "")
	if payload.Code == "" {
		// A code-less error cannot be translated by the UI.
		t.Fatalf("login error carried no code: %q", body)
	}
	return payload.Code
}

func TestHandleWorkBuddyLogin_RejectsCrossOriginStart(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})
	req := channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", "")
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()

	a.HandleWorkBuddyLogin(rec, req)
	testutil.Equal(t, rec.Code, http.StatusForbidden)
	testutil.Equal(t, decodeLoginError(t, rec.Body.String()), "origin_mismatch")
	testutil.MustNotContain(t, rec.Body.String(), "state")
}

func TestHandleWorkBuddyLogin_RejectsInsecureOrigin(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})
	req := channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", "")
	req.Host = "admin.example.com"
	req.Header.Set("Origin", "http://admin.example.com")
	rec := httptest.NewRecorder()

	a.HandleWorkBuddyLogin(rec, req)
	testutil.Equal(t, rec.Code, http.StatusForbidden)
	testutil.Equal(t, decodeLoginError(t, rec.Body.String()), "insecure_origin")
}

func TestHandleWorkBuddyLogin_ReportsUnreachableUpstream(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	// A closed listener stands in for a blocked or unreachable upstream.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	stubWorkBuddyLoginClient(t, deadURL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))

	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.Equal(t, decodeLoginError(t, rec.Body.String()), "upstream_unreachable")
}

func TestHandleWorkBuddyLogin_ReportsUpstreamRejection(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"code":40301,"msg":"forbidden"}`))
	}))
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))

	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.Equal(t, decodeLoginError(t, rec.Body.String()), "upstream_rejected")
	testutil.MustNotContain(t, rec.Body.String(), "40301")
}

func TestHandleWorkBuddyLogin_RequiresStore(t *testing.T) {
	t.Parallel()

	a := New(nil, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)
}

func TestHandleWorkBuddyLogin_RejectsNonJSONBody(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})
	req := channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", `enabled=true`)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	a.HandleWorkBuddyLogin(rec, req)
	testutil.Equal(t, rec.Code, http.StatusUnsupportedMediaType)
}

func TestHandleWorkBuddyLogin_StartReturnsOfficialLoginURL(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := workbuddyAuthServer(t, []string{`{"code":11217,"msg":"11217:login ing..."}`}, workbuddyCatalogBody)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", `{"enabled":true}`))

	testutil.Equal(t, rec.Code, http.StatusOK)
	var response struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresAt               string `json:"expires_at"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response), "decode: %v")
	testutil.Falsef(t, response.ID == "" || response.Status != "pending", "response = %+v", response)
	parsed, err := url.Parse(response.VerificationURIComplete)
	testutil.NoError(t, err, "login URL is not parseable: %v")
	testutil.Equal(t, parsed.Host, "www.workbuddy.ai")
	testutil.Equal(t, parsed.Path, "/login")
	testutil.Equal(t, parsed.Query().Get("state"), "state-123")
	testutil.Equal(t, parsed.Query().Get("version"), workbuddy.LoginClientVersion)

	// The pending transaction must be pollable. The upstream state is part of
	// the official login URL the browser needs, but the transport-level fields
	// (device code / user code) must stay empty for this flow.
	rec2 := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec2, channelLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+response.ID, ""))
	testutil.Equal(t, rec2.Code, http.StatusOK)
	var polled deviceLoginResponse
	testutil.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &polled), "decode poll: %v")
	testutil.Equal(t, polled.UserCode, "")
	testutil.Falsef(t, !strings.HasPrefix(polled.VerificationURIComplete, "https://www.workbuddy.ai/login?"), "poll returned an unexpected login URL: %q", polled.VerificationURIComplete)
}

func TestHandleWorkBuddyLogin_CompletesAndPersistsAccount(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := workbuddyAuthServer(t, []string{
		`{"code":11217,"msg":"11217:login ing..."}`,
		`{"code":0,"data":{"accessToken":"` + testJWT("uid-abc", "operator@example.com") + `","refreshToken":"refresh-abc","expiresIn":31536000}}`,
	}, workbuddyCatalogBody)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))

	var started struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "start response = %q", rec.Body.String())

	deadline := time.Now().Add(10 * time.Second)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleWorkBuddyLogin(pollRec, channelLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+started.ID, ""))
		testutil.Equal(t, pollRec.Code, http.StatusOK)
		testutil.NoError(t, json.Unmarshal(pollRec.Body.Bytes(), &final), "decode poll: %v")
		if final.Status != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "complete")
	testutil.NotEqual(t, final.AccountID, 0)

	acc, err := s.GetAccount(context.Background(), final.AccountID)
	testutil.NoError(t, err, "GetAccount() error = %v")
	testutil.Equal(t, acc.AccountType, "workbuddy")
	testutil.Equal(t, acc.WorkBuddyRefreshToken, "refresh-abc")
	testutil.Equal(t, acc.WorkBuddyUID, "uid-abc")
	testutil.Equal(t, acc.Email, "operator@example.com")
	testutil.Equal(t, len(acc.WorkBuddyModelIDs), 2)
	testutil.Equal(t, acc.ClientCookie, "")
	testutil.Equal(t, acc.Token, "")
	testutil.Equal(t, acc.RefreshToken, "")

	// A second login for the same credential must update, not duplicate.
	auth2 := workbuddyAuthServer(t, []string{
		`{"code":0,"data":{"accessToken":"` + testJWT("uid-abc", "operator@example.com") + `","refreshToken":"refresh-abc","expiresIn":31536000}}`,
	}, workbuddyCatalogBody)
	defer auth2.Close()
	stubWorkBuddyLoginClient(t, auth2.URL)

	rec3 := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec3, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	var second struct {
		ID string `json:"id"`
	}
	err = json.Unmarshal(rec3.Body.Bytes(), &second)
	testutil.Falsef(t, err != nil || second.ID == "", "second start response = %q", rec3.Body.String())
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleWorkBuddyLogin(pollRec, channelLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+second.ID, ""))
		if err := json.Unmarshal(pollRec.Body.Bytes(), &final); err != nil {
			break
		}
		if final.Status != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "complete")
	testutil.Equal(t, final.AccountID, acc.ID)
	accounts, err := s.ListAccounts(context.Background())
	testutil.NoError(t, err, "ListAccounts() error = %v")
	workbuddyAccounts := 0
	for _, candidate := range accounts {
		if strings.EqualFold(candidate.AccountType, "workbuddy") {
			workbuddyAccounts++
		}
	}
	testutil.Equal(t, workbuddyAccounts, 1)
}

func TestHandleWorkBuddyLogin_ReportsVerificationFailure(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	// A token that cannot list the catalog must not be persisted.
	auth := workbuddyAuthServer(t, []string{
		`{"code":0,"data":{"accessToken":"` + testJWT("uid-abc", "operator@example.com") + `","refreshToken":"refresh-abc","expiresIn":3600}}`,
	}, `{"code":6004,"msg":"6004:usage exceeds frequency limit"}`)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "start response = %q", rec.Body.String())

	var final deviceLoginResponse
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleWorkBuddyLogin(pollRec, channelLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+started.ID, ""))
		testutil.NoError(t, json.Unmarshal(pollRec.Body.Bytes(), &final), "decode poll: %v")
		if final.Status != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "failed")
	accounts, err := s.ListAccounts(context.Background())
	testutil.NoError(t, err, "ListAccounts() error = %v")
	for _, candidate := range accounts {
		testutil.Falsef(t, strings.EqualFold(candidate.AccountType, "workbuddy"), "an unverified account was persisted: %+v", candidate)
	}
}

func TestHandleWorkBuddyLogin_CancelForgetsTransaction(t *testing.T) {
	s, _ := newTestStore(t, "wb-login:")
	auth := workbuddyAuthServer(t, []string{`{"code":11217,"msg":"11217:login ing..."}`}, workbuddyCatalogBody)
	defer auth.Close()
	stubWorkBuddyLoginClient(t, auth.URL)

	a := New(s, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/workbuddy/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "start response = %q", rec.Body.String())

	deleteRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(deleteRec, channelLoginRequest(t, http.MethodDelete, "/api/workbuddy/login/"+started.ID, ""))
	testutil.Equal(t, deleteRec.Code, http.StatusNoContent)

	missingRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(missingRec, channelLoginRequest(t, http.MethodGet, "/api/workbuddy/login/"+started.ID, ""))
	testutil.Equal(t, missingRec.Code, http.StatusNotFound)
}

func TestHandleWorkBuddyLogin_RejectsUnknownMethodAndMissingSession(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t, "wb-login:")
	a := New(s, "", "", &config.Config{})

	putRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(putRec, channelLoginRequest(t, http.MethodPut, "/api/workbuddy/login", "{}"))
	testutil.Equal(t, putRec.Code, http.StatusMethodNotAllowed)

	missingRec := httptest.NewRecorder()
	a.HandleWorkBuddyLogin(missingRec, channelLoginRequest(t, http.MethodGet, "/api/workbuddy/login/does-not-exist", ""))
	testutil.Equal(t, missingRec.Code, http.StatusNotFound)
}
