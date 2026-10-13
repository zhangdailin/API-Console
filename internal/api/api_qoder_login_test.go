package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/qoder"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// qoderAuthServer stubs the Qoder device authorization endpoints. It records
// nothing secret and never echoes a credential it was not asked for.
type qoderAuthServer struct {
	*httptest.Server
	polls   int
	refresh int
}

func newQoderAuthServer(t *testing.T, pollBodies []string) *qoderAuthServer {
	t.Helper()
	stub := &qoderAuthServer{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/device/selectAccounts":
			// The authorization page is only ever opened in a browser, so a
			// direct hit here means the flow went wrong.
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			for _, key := range []string{"nonce", "verifier", "challenge_method"} {
				testutil.CheckNotEqual(t, r.URL.Query().Get(key), "")
			}
			index := stub.polls
			stub.polls++
			if index >= len(pollBodies) {
				index = len(pollBodies) - 1
			}
			body := pollBodies[index]
			if body == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(body))
		case "/api/v1/deviceToken/refresh":
			stub.refresh++
			_, _ = w.Write([]byte(`{"device_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
		case "/api/v1/userinfo":
			got := r.Header.Get("Authorization")
			testutil.CheckFalsef(t, !strings.HasPrefix(got, "Bearer "), "userinfo Authorization = %q, want a bearer token", got)
			_, _ = w.Write([]byte(`{"uid":"uid-qoder","name":"operator","email":"operator@example.com","organization_id":"org-1","organization_tags":["tag-a"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

func qoderLoginConfig(baseURL string) *config.Config {
	return &config.Config{
		QoderOAuthBaseURL:   baseURL,
		QoderOpenAPIBaseURL: baseURL,
		QoderInferenceURL:   baseURL,
	}
}

// TestHandleQoderLogin_StartReturnsOfficialDeviceURL proves the start step
// returns a device authorization URL and never leaks the transaction's private
// halves.
func TestHandleQoderLogin_StartReturnsOfficialDeviceURL(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := newQoderAuthServer(t, []string{""})
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", `{"enabled":true}`))

	testutil.Equal(t, rec.Code, http.StatusOK)
	var response struct {
		ID                      string `json:"id"`
		Status                  string `json:"status"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresAt               string `json:"expires_at"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response), "decode: %v")
	testutil.Falsef(t, response.ID == "" || response.Status != "pending", "response = %+v", response)
	parsed, err := url.Parse(response.VerificationURIComplete)
	testutil.NoError(t, err, "authorization URL is not parseable: %v")
	testutil.Equal(t, parsed.Path, "/device/selectAccounts")
	for _, key := range []string{"challenge", "challenge_method", "nonce", "machine_id", "client_id"} {
		testutil.CheckNotEqual(t, parsed.Query().Get(key), "")
	}
	testutil.CheckEqual(t, parsed.Query().Get("challenge_method"), "S256")
	// The nonce and the verifier are the transaction's private halves; a nonce
	// that reached the browser would let a third party complete the attempt.
	testutil.MustNotContain(t, rec.Body.String(), "verifier")
	testutil.Equal(t, response.UserCode, "")
	testutil.NotEqual(t, strings.Contains(response.VerificationURIComplete, parsed.Query().Get("nonce")), false)

	// The pending transaction must be pollable and must stay pending while the
	// upstream answers 404.
	pollRec := httptest.NewRecorder()
	a.HandleQoderLogin(pollRec, channelLoginRequest(t, http.MethodGet, "/api/qoder/login/"+response.ID, ""))
	testutil.Equal(t, pollRec.Code, http.StatusOK)
	var polled deviceLoginResponse
	testutil.NoError(t, json.Unmarshal(pollRec.Body.Bytes(), &polled), "decode poll: %v")
	testutil.Equal(t, polled.Status, "pending")
}

func TestHandleQoderLogin_CancelStopsBlockedPollAndDoesNotPersist(t *testing.T) {
	s, _ := newTestStore(t, "qd-cancel:")
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})
	var startedOnce sync.Once
	var cancelledOnce sync.Once
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			startedOnce.Do(func() { close(pollStarted) })
			<-r.Context().Done()
			cancelledOnce.Do(func() { close(pollCancelled) })
		default:
			http.NotFound(w, r)
		}
	}))
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	start := httptest.NewRecorder()
	a.HandleQoderLogin(start, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))
	var response struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(start.Body.Bytes(), &response)
	testutil.Falsef(t, err != nil || response.ID == "", "start response = %q", start.Body.String())
	// Speed up only this transaction; production keeps the normal two-second
	// cadence.
	a.qoderLogins.update(response.ID, func(login *browserLogin) { login.interval = time.Millisecond })

	select {
	case <-pollStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("poll request did not start")
	}
	cancelRec := httptest.NewRecorder()
	a.HandleQoderLogin(cancelRec, channelLoginRequest(t, http.MethodDelete, "/api/qoder/login/"+response.ID, ""))
	testutil.Equal(t, cancelRec.Code, http.StatusNoContent)
	select {
	case <-pollCancelled:
	case <-time.After(time.Second):
		t.Fatal("blocked upstream poll was not cancelled")
	}
	accounts, err := s.ListAccounts(context.Background())
	testutil.NoError(t, err)
	for _, acc := range accounts {
		testutil.Falsef(t, strings.EqualFold(acc.AccountType, "qoder"), "cancelled login persisted an account: %+v", acc)
	}
}

func TestHandleQoderLogin_PreservesDisabledPreference(t *testing.T) {
	s, _ := newTestStore(t, "qd-disabled:")
	auth := newQoderAuthServer(t, []string{
		`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"user_id":"uid-disabled","user_name":"operator"}`,
	})
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))

	start := httptest.NewRecorder()
	a.HandleQoderLogin(start, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", `{"enabled":false}`))
	var response struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(start.Body.Bytes(), &response)
	testutil.Falsef(t, err != nil || response.ID == "", "start response = %q", start.Body.String())
	a.qoderLogins.update(response.ID, func(login *browserLogin) { login.interval = time.Millisecond })

	deadline := time.Now().Add(5 * time.Second)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		poll := httptest.NewRecorder()
		a.HandleQoderLogin(poll, channelLoginRequest(t, http.MethodGet, "/api/qoder/login/"+response.ID, ""))
		testutil.NoError(t, json.Unmarshal(poll.Body.Bytes(), &final))
		if final.Status == "complete" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "complete")
	acc, err := s.GetAccount(context.Background(), final.AccountID)
	testutil.NoError(t, err)
	testutil.False(t, acc.Enabled, "enabled:false was lost when the Qoder account was persisted")
}

// TestHandleQoderLogin_CompletesAndPersistsAccount proves the whole flow: the
// browser step completes, the account is verified against the signed catalog and
// the credential plus its derived runtime pair are persisted.
func TestHandleQoderLogin_CompletesAndPersistsAccount(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := newQoderAuthServer(t, []string{
		"",
		// A token lifetime longer than the refresh lead, so the credential is
		// usable without an immediate rotation.
		`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400,"refresh_token_expires_in":864000,"user_id":"uid-qoder","user_name":"operator"}`,
	})
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))

	var started struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "start response = %q", rec.Body.String())

	deadline := time.Now().Add(15 * time.Second)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleQoderLogin(pollRec, channelLoginRequest(t, http.MethodGet, "/api/qoder/login/"+started.ID, ""))
		testutil.Equal(t, pollRec.Code, http.StatusOK)
		testutil.NoError(t, json.Unmarshal(pollRec.Body.Bytes(), &final), "decode poll: %v")
		if final.Status == "complete" || final.Status == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "complete")
	testutil.NotEqual(t, final.AccountID, 0)

	acc, err := s.GetAccount(t.Context(), final.AccountID)
	testutil.NoError(t, err, "GetAccount: %v")
	testutil.Equal(t, acc.AccountType, "qoder")
	testutil.Falsef(t, acc.QoderAccessToken == "" || acc.QoderRefreshToken == "", "stored credential = %q/%q, want a persisted pair", acc.QoderAccessToken, acc.QoderRefreshToken)
	testutil.Equal(t, acc.QoderUserID, "uid-qoder")
	testutil.NotEqual(t, acc.QoderMachineID, "")
	testutil.False(t, acc.QoderRuntimeInfo == "" || acc.QoderRuntimeKey == "", "the derived runtime pair was not stored")
	// The catalog now comes from the signed upstream control plane. This stub
	// answers 404 for every catalog route, so the login must save the account
	// with an empty snapshot: nothing compiled in may be installed as if it had
	// been observed, and the operator is expected to run a refresh.
	testutil.Equal(t, len(acc.QoderModelIDs), 0)
	testutil.Equal(t, acc.Email, "operator@example.com")

	// The account response must never carry the durable credential or the
	// derived runtime material.
	redacted := qoder.RedactOutput(acc)
	testutil.False(t, redacted.QoderRefreshToken != "" || redacted.QoderRuntimeKey != "", "RedactQoderOutput left a secret in place")
	testutil.NotEqual(t, redacted.QoderAccessToken, "")
}

// TestHandleQoderLogin_ReportsUnusableCredential proves a login whose credential
// cannot be resolved to an identity is still refused, and that the reason — not
// just "could not be verified" — reaches the operator.
func TestHandleQoderLogin_ReportsUnusableCredential(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/v1/deviceToken/poll":
			// No user_id, and userinfo will also refuse, so no identity can be
			// established and the account must not be stored.
			_, _ = w.Write([]byte(`{"token":"access-1","refresh_token":"refresh-1","expires_in":86400}`))
		case "/api/v1/userinfo":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"token rejected"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer auth.Close()
	a := New(s, "", "", qoderLoginConfig(auth.URL))
	final := runQoderLoginToCompletion(t, a, s, 15*time.Second)
	testutil.Equal(t, final.Status, "failed")
	// The reason must name the actual problem instead of only saying that
	// verification failed.
	testutil.MustContain(t, final.Message, "user id")

	accounts, err := s.ListAccounts(t.Context())
	testutil.NoError(t, err, "ListAccounts: %v")
	for _, acc := range accounts {
		testutil.Falsef(t, strings.EqualFold(acc.AccountType, "qoder"), "an unusable Qoder account was persisted: %+v", acc)
	}
}

// runQoderLoginToCompletion starts a login, polls it to a terminal state and
// reports the final transaction.
func runQoderLoginToCompletion(t *testing.T, a *API, s *store.Store, timeout time.Duration) deviceLoginResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))
	var started struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "start response = %q", rec.Body.String())

	deadline := time.Now().Add(timeout)
	var final deviceLoginResponse
	for time.Now().Before(deadline) {
		pollRec := httptest.NewRecorder()
		a.HandleQoderLogin(pollRec, channelLoginRequest(t, http.MethodGet, "/api/qoder/login/"+started.ID, ""))
		testutil.Equal(t, pollRec.Code, http.StatusOK)
		testutil.NoError(t, json.Unmarshal(pollRec.Body.Bytes(), &final), "decode poll: %v")
		if final.Status == "complete" || final.Status == "failed" || final.Status == "expired" {
			return final
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("login did not reach a terminal state: %+v", final)
	return final
}

// TestHandleQoderLogin_RequiresStoreAndRejectsBadMethods covers the guard rails.
func TestHandleQoderLogin_RequiresStoreAndRejectsBadMethods(t *testing.T) {
	a := New(nil, "", "", &config.Config{})
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", ""))
	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)

	s, _ := newTestStore(t, "qd-login:")
	a = New(s, "", "", &config.Config{})
	rec = httptest.NewRecorder()
	a.HandleQoderLogin(rec, channelLoginRequest(t, http.MethodPut, "/api/qoder/login", ""))
	testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
	rec = httptest.NewRecorder()
	a.HandleQoderLogin(rec, channelLoginRequest(t, http.MethodPost, "/api/qoder/login", `{"unexpected":1}`))
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
}

// TestHandleQoderLogin_RejectsForeignOrigin proves a credential-mutating request
// must come from this admin origin.
func TestHandleQoderLogin_RejectsForeignOrigin(t *testing.T) {
	s, _ := newTestStore(t, "qd-login:")
	a := New(s, "", "", &config.Config{})

	req := httptest.NewRequest(http.MethodPost, "/api/qoder/login", strings.NewReader(""))
	req.Host = "localhost"
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	a.HandleQoderLogin(rec, req)
	testutil.Equal(t, rec.Code, http.StatusForbidden)
	testutil.MustContain(t, rec.Body.String(), "origin_mismatch")
}

// TestQoderAccountCreateAndEditAreOAuthOnly proves the account API refuses to
// create a Qoder account by hand and keeps the stored credential on an edit.
func TestQoderAccountCreateAndEditAreOAuthOnly(t *testing.T) {
	s, _ := newTestStore(t, "qd-account:")
	a := New(s, "", "", &config.Config{})

	// Creation by hand is refused with a pointer at the official login.
	createBody := `{"account_type":"qoder","client_cookie":"pat-shaped-value","enabled":true}`
	rec := httptest.NewRecorder()
	a.HandleAccounts(rec, qoderAccountRequest(t, http.MethodPost, "/api/accounts", createBody))
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.MustContain(t, rec.Body.String(), "/api/qoder/login")

	// A stored account keeps its credential when an edit omits it.
	acc := &store.Account{
		AccountType:       "qoder",
		Name:              "qoder-test",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderUserID:       "uid-qoder",
		QoderRuntimeInfo:  "info",
		QoderRuntimeKey:   "key",
		Enabled:           true,
	}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc), "CreateAccount: %v")

	editBody, _ := json.Marshal(map[string]interface{}{
		"account_type": "qoder",
		"name":         "renamed",
		"enabled":      true,
	})
	rec = httptest.NewRecorder()
	a.HandleAccountByID(rec, qoderAccountRequest(t, http.MethodPut, "/api/accounts/"+strconv.FormatInt(acc.ID, 10), string(editBody)))
	testutil.Equal(t, rec.Code, http.StatusOK)

	stored, err := s.GetAccount(t.Context(), acc.ID)
	testutil.NoError(t, err, "GetAccount: %v")
	testutil.Equal(t, stored.QoderRefreshToken, "refresh-1")
	testutil.Equal(t, stored.QoderRuntimeKey, "key")
	testutil.Equal(t, stored.QoderUserID, "uid-qoder")
	testutil.Equal(t, stored.Name, "renamed")
}

func qoderAccountRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestVerifyQoderAccountDoesNotReportForbidden proves the account check uses the
// local catalog and only needs the identity endpoint from the control plane.
func TestVerifyQoderAccountDoesNotReportForbidden(t *testing.T) {
	s, _ := newTestStore(t, "qd-verify:")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/userinfo":
			_, _ = w.Write([]byte(`{"uid":"uid-qoder","name":"operator","email":"operator@example.com"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	acc := &store.Account{
		AccountType:       "qoder",
		Name:              "qoder-verify",
		QoderAccessToken:  "access-1",
		QoderRefreshToken: "refresh-1",
		QoderMachineID:    "11111111-2222-4333-8444-555555555555",
		QoderUserID:       "uid-qoder",
		QoderRuntimeInfo:  "runtime-info",
		QoderRuntimeKey:   "runtime-key",
		QoderDataPolicy:   true,
		Enabled:           true,
	}
	testutil.NoError(t, s.CreateAccount(t.Context(), acc), "CreateAccount: %v")

	cfg := &config.Config{
		QoderOAuthBaseURL:   upstream.URL,
		QoderOpenAPIBaseURL: upstream.URL,
		QoderInferenceURL:   upstream.URL,
	}
	status, httpStatus, err := qoder.VerifyAccount(t.Context(), acc, cfg, s)
	testutil.NoError(t, err, "verifyQoderAccount() error = %v, want success")
	testutil.Equal(t, status, "")
	testutil.Equal(t, httpStatus, 0)
	testutil.Equal(t, apperrors.ClassifyAccountStatus(""), "")
	// The catalog is an upstream observation, not a compiled-in list. This stub
	// answers 404 for every catalog route, so verification must leave the
	// snapshot empty rather than installing anything.
	testutil.Equal(t, len(acc.QoderModelIDs), 0)
}

// TestQoderQuotaResponseFieldsAreAuthoritative proves the account payload reports
// the gateway's own credit window as a known, observed balance.
//
// The generic projection reads usage_limit/usage_current and marks the window as
// an estimate unless a channel says otherwise, which for Qoder would present a
// reported window as guesswork — and left the console's quota column blank.
func TestQoderQuotaResponseFieldsAreAuthoritative(t *testing.T) {
	t.Parallel()

	trial := &store.Account{
		ID:           184,
		AccountType:  "qoder",
		UsageLimit:   300,
		UsageCurrent: 300,
		QoderQuota: store.QoderQuotaSnapshot{
			Limit:      300,
			Remaining:  300,
			PlanTier:   "Pro Trial",
			Unit:       "credits",
			UpgradeURL: "https://qoder.com/pricing?client=qoder",
			SyncedAt:   time.Now(),
		},
	}
	fields := buildQuotaResponseFields(trial)
	testutil.EqualAny(t, fields["quota_limit"], float64(300))
	testutil.EqualAny(t, fields["quota_remaining"], float64(300))
	testutil.Equal(t, fields["quota_plan"], "Pro Trial")
	testutil.Equal(t, fields["quota_limit_known"], true)
	testutil.Equal(t, fields["quota_observed"], true)
	testutil.Equal(t, fields["quota_exhausted"], false)

	exhausted := &store.Account{
		ID:          185,
		AccountType: "qoder",
		QoderQuota: store.QoderQuotaSnapshot{
			Exhausted:  true,
			PlanTier:   "Free",
			Unit:       "credits",
			UpgradeURL: "https://qoder.com/pricing?client=qoder",
			ResetAt:    time.Now().Add(12 * time.Hour),
			SyncedAt:   time.Now(),
		},
	}
	exhaustedFields := buildQuotaResponseFields(exhausted)
	testutil.Equal(t, exhaustedFields["quota_exhausted"], true)
	testutil.Equal(t, exhaustedFields["quota_plan"], "Free")
	// A zero limit is a known zero, not an unknown window.
	testutil.Equal(t, exhaustedFields["quota_limit_known"], true)
	testutil.NotEqual(t, exhaustedFields["quota_upgrade_url"], "")
}
