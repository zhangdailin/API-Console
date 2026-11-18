package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVerificationURLsRejectUntrustedOrigins(t *testing.T) {
	for _, u := range []string{"http://app.warp.dev/x", "https://evil.example/x", "https://app.warp.dev.evil.example/x", "https://user@app.warp.dev/x", "https://app.warp.dev:8443/x"} {
		if validVerificationURL(u) {
			t.Fatal("accepted untrusted URL")
		}
	}
	if !validVerificationURL("https://app.warp.dev/device?user_code=TEST") {
		t.Fatal("official URL rejected")
	}
}
func TestHTTPFailuresNeverEchoSecrets(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"private-token-and-key"}`))
	}))
	defer s.Close()
	c := &probeClient{http: s.Client(), base: s.URL}
	var result any
	e := c.json(context.Background(), s.URL, "application/json", nil, "test-token", &result)
	if e == nil || strings.Contains(e.Error(), "private") || strings.Contains(e.Error(), "test-token") {
		t.Fatal("unsafe diagnostic")
	}
}
func TestPendingAndSlowDown(t *testing.T) {
	for _, code := range []string{"authorization_pending", "slow_down"} {
		t.Run(code, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
			}))
			defer s.Close()
			c := &probeClient{http: s.Client()}
			var d any
			e := c.json(context.Background(), s.URL, "application/json", nil, "", &d)
			if !errors.Is(e, pendingAuthorization) && !errors.Is(e, slowAuthorization) {
				t.Fatal("pending response misclassified")
			}
		})
	}
}
func TestCatalogFiltersDisabledAndRejectsFallback(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"defaultId":"model-a","choices":[{"id":"model-a","disableReason":""},{"id":"model-b","disableReason":"upgrade"},{"id":"model-a"}]}}}]}}}}`))
	}))
	defer s.Close()
	c := &probeClient{http: s.Client(), base: s.URL}
	rows, selected, e := c.models(context.Background(), "test-token")
	if e != nil || len(rows) != 1 || selected != "model-a" {
		t.Fatal("catalog filter mismatch")
	}
}
func TestDeviceExchangeUsesOfficialFormWithoutReturningRefreshCredential(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		switch r.URL.Path {
		case "/api/v1/oauth/token":
			if r.Form.Get("device_code") != "test-device" || r.Form.Get("client_id") != "warp-agent-cli" {
				t.Error("wrong OAuth fields")
			}
			_, _ = w.Write([]byte(`{"access_token":"test-custom-token"}`))
		case "/custom":
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.Form.Get("token") != "test-custom-token" || r.Form.Get("returnSecureToken") != "true" {
				t.Error("wrong Firebase form")
			}
			_, _ = w.Write([]byte(`{"idToken":"test-id-token","refreshToken":"must-not-return"}`))
		default:
			t.Error("unexpected path")
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := &probeClient{http: server.Client(), base: server.URL, customURL: server.URL + "/custom"}
	token, err := c.exchange(context.Background(), device{DeviceCode: "test-device"})
	if err != nil || token != "test-id-token" {
		t.Fatal("device exchange failed")
	}
}

func TestOAuthOnlyNeverCallsFirebaseCatalogOrGeneration(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/oauth/token" {
			t.Error("OAuth-only reached another stage")
			w.WriteHeader(400)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "warp-agent-cli" || r.Form.Get("device_code") != "test-device" {
			t.Error("wrong device authorization")
		}
		_, _ = w.Write([]byte(`{"access_token":"test-private-custom"}`))
	}))
	defer server.Close()
	client := &probeClient{http: server.Client(), base: server.URL, customURL: server.URL + "/must-not-call"}
	grant, err := client.exchangeCustom(context.Background(), device{DeviceCode: "test-device"})
	if err != nil || grant != "test-private-custom" || calls != 1 {
		t.Fatal("OAuth-only exchange mismatch")
	}
}

func TestRedirectNeverForwardsAuthorization(t *testing.T) {
	forwarded := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := newProbeClient("synthetic-project-config")
	var result any
	err := client.json(context.Background(), source.URL, "application/json", nil, "synthetic-bearer", &result)
	if err == nil || forwarded {
		t.Fatal("redirected authenticated request escaped original endpoint")
	}
	if strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), source.URL) {
		t.Fatal("sensitive redirect diagnostic")
	}
}

func TestGenerationNonSSEIsNotSuccess(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("login"))
	}))
	defer s.Close()
	c := &probeClient{http: s.Client(), base: s.URL}
	_, e := c.generate(context.Background(), "test-token", "model-a")
	if e == nil {
		t.Fatal("HTML accepted as generation")
	}
}
