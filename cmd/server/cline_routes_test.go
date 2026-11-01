package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/modelrefresh"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
)

// clineE2EStub serves every endpoint the Cline channel touches, so a request can
// travel the real route table, the real handler and the real client.
type clineE2EStub struct {
	*httptest.Server
	chatHeaders http.Header
	chatBody    []byte
	chatCalls   int
	feedCalls   int
	polls       int
}

func newClineE2EStub(t *testing.T) *clineE2EStub {
	t.Helper()
	stub := &clineE2EStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user_management/authorize/device":
			page := "http://" + r.Host + "/device"
			_, _ = w.Write([]byte(`{"device_code":"dev-e2e","user_code":"ABCD-EFGH",` +
				`"verification_uri":"` + page + `",` +
				`"verification_uri_complete":"` + page + `?code=ABCD-EFGH",` +
				`"interval":1,"expires_in":900}`))
		case "/user_management/authenticate":
			stub.polls++
			if stub.polls < 2 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"workos-access","refresh_token":"workos-refresh"}`))
		case "/api/v1/auth/register":
			_, _ = w.Write([]byte(`{"data":{"accessToken":"cline-access","refreshToken":"cline-refresh",` +
				`"expiresAt":4102444800000,"userInfo":{"email":"e2e@example.com"}}}`))
		case "/api/v1/ai/cline/recommended-models":
			stub.feedCalls++
			_, _ = w.Write([]byte(`{"free":[{"id":"x-ai/grok-4.1-fast","name":"Grok 4.1 Fast"}]}`))
		case "/api/v1/chat/completions":
			stub.chatCalls++
			stub.chatHeaders = r.Header.Clone()
			raw, _ := io.ReadAll(r.Body)
			stub.chatBody = raw
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"e2e answer\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	return stub
}

// TestClineChannelEndToEnd drives one chat completion through the registered
// route table with a Cline account the WorkOS login flow created.
//
// This is the integration the unit tests cannot reach: route table, channel
// detection, account selection, client construction, the request credential and
// the SSE conversion all run together. The requests go through a real HTTP
// server so the same lifecycle the deployment uses is exercised.
func TestClineChannelEndToEnd(t *testing.T) {

	stub := newClineE2EStub(t)
	defer stub.Close()

	const managedKey = "sk-cline-e2e"
	cfg := &config.Config{
		AdminUser:               "admin",
		AdminPass:               "secret",
		AdminToken:              "admintoken",
		AdminPath:               "/admin",
		ClineAPIBaseURL:         stub.URL + "/api/v1",
		ClineWorkOSAuthorizeURL: stub.URL + "/user_management/authorize/device",
		ClineWorkOSTokenURL:     stub.URL + "/user_management/authenticate",
	}

	e := newChannelE2E(t, "cline-e2e:", managedKey, cfg)

	// 1. Create the account through the WorkOS device authorization flow.
	startBody := e.readBody(t, e.do(t, http.MethodPost, "/api/cline/login", "", true))
	var started struct {
		ID                      string `json:"id"`
		VerificationURIComplete string `json:"verification_uri_complete"`
	}
	err := json.Unmarshal([]byte(startBody), &started)
	testutil.Falsef(t, err != nil || started.ID == "", "login start response = %q", startBody)
	testutil.NotEqual(t, started.VerificationURIComplete, "")

	var final struct {
		Status    string `json:"status"`
		AccountID int64  `json:"account_id"`
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pollBody := e.readBody(t, e.do(t, http.MethodGet, "/api/cline/login/"+started.ID, "", true))
		err := json.Unmarshal([]byte(pollBody), &final)
		testutil.CheckNoError(t, err)
		if final.Status == "complete" || final.Status == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	testutil.Equal(t, final.Status, "complete")

	// 2. Refresh the channel catalog from the account.
	refreshBody := e.readBody(t, e.do(t, http.MethodPost, "/api/models/refresh?channel=cline", "", true))
	var refreshed modelrefresh.Result
	err = json.Unmarshal([]byte(refreshBody), &refreshed)
	testutil.CheckNoError(t, err)
	testutil.Falsef(t, refreshed.Channel != "Cline" || refreshed.Discovered == 0, "refresh result = %+v, want a discovered Cline catalog", refreshed)
	// The catalog must have come from the upstream feed, never a compiled-in
	// list.
	testutil.Equal(t, refreshed.Source, "cline_recommended_models")
	testutil.NotEqual(t, stub.feedCalls, 0)

	// 3. Run one chat completion through the channel route.
	chatBody := e.readBody(t, e.do(t, http.MethodPost, "/cline/v1/chat/completions",
		`{"model":"x-ai/grok-4.1-fast","stream":true,"messages":[{"role":"user","content":"hello"}]}`, false))
	testutil.MustContain(t, chatBody, "e2e answer")
	testutil.Equal(t, stub.chatCalls, 1)

	// The request credential is the Cline pair with the literal workos: prefix,
	// and the task id doubles as the session id.
	testutil.CheckEqual(t, stub.chatHeaders.Get("Authorization"), "Bearer workos:cline-access")
	testutil.CheckNotEqual(t, stub.chatHeaders.Get("X-Task-ID"), "")
	// The WorkOS tokens are the exchanged halves, never the request credential.
	testutil.MustNotContain(t, fmtHeaders(stub.chatHeaders), "workos-access")
	testutil.MustContain(t, string(stub.chatBody), `"session_id"`)
	testutil.MustContain(t, string(stub.chatBody), `"reasoning_effort":"high"`)
}

// fmtHeaders renders headers as text so a test can assert a value never
// appears in any of them.
func fmtHeaders(headers http.Header) string {
	var builder strings.Builder
	for name, values := range headers {
		builder.WriteString(name)
		builder.WriteString(":")
		builder.WriteString(strings.Join(values, " "))
		builder.WriteString("\n")
	}
	return builder.String()
}
