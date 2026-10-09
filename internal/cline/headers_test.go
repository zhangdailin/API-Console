package cline

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// TestChatRequestCarriesTheClineProductIdentity pins the identity block the
// upstream gates the free catalog on.
//
// Without it the API answers 403 with "<model> is only available via Cline
// product surfaces": the refusal names a client, not a credential, so no amount
// of re-login repairs it. The header set is therefore part of the wire contract
// and is asserted like one.
func TestChatRequestCarriesTheClineProductIdentity(t *testing.T) {
	t.Parallel()

	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	client := &Client{
		apiBase: server.URL,
		stream:  server.Client(),
		creds:   Credentials{AccessToken: "access-1"},
		account: &store.Account{ClineModelIDs: []string{`{"id":"cline-free/deepseek-v4.1-flash"}`}},
	}
	if err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "cline-free/deepseek-v4.1-flash",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "ping"}}},
	}, nil, nil); err != nil {
		t.Fatalf("SendRequestWithPayload() error = %v", err)
	}

	for name, want := range defaultClientHeaders {
		testutil.CheckEqual(t, seen.Get(name), want)
	}
	testutil.CheckNotEqual(t, seen.Get("X-Task-ID"), "")
	testutil.CheckEqual(t, seen.Get("Authorization"), "Bearer workos:access-1")
}

// TestCatalogRequestCarriesTheClineProductIdentity keeps the model feed on the
// same identity: it is read with the same account token and is served by the
// same product gate.
func TestCatalogRequestCarriesTheClineProductIdentity(t *testing.T) {
	t.Parallel()

	var seen http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"free":[{"id":"cline-free/deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash"}]}`))
	}))
	defer server.Close()

	client := &Client{
		apiBase: server.URL,
		control: server.Client(),
		creds:   Credentials{AccessToken: "access-1"},
	}
	_, err := client.FetchUpstreamModels(context.Background())
	testutil.CheckNoError(t, err)
	for name, want := range defaultClientHeaders {
		testutil.CheckEqual(t, seen.Get(name), want)
	}
}

// TestApplyClientHeadersKeepsAnExplicitValue lets a caller-supplied header win,
// so an override or a deliberately different Accept is not silently replaced.
func TestApplyClientHeadersKeepsAnExplicitValue(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	h.Set("Accept", "application/json")
	applyClientHeaders(h)
	testutil.CheckEqual(t, h.Get("Accept"), "application/json")
	testutil.CheckEqual(t, h.Get("X-CLIENT-TYPE"), "cline-cli")
}

// TestChat401RefreshRetryRebuildsThePOSTBody guards the consumed-body bug from
// the reference proxy's PR #4: every attempt must create a fresh POST request.
func TestChat401RefreshRetryRebuildsThePOSTBody(t *testing.T) {
	t.Parallel()

	var chatAttempts atomic.Int32
	var refreshAttempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			refreshAttempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"accessToken":"access-2","refreshToken":"refresh-2","expiresAt":"2099-01-01T00:00:00Z"}}`)
		case "/chat/completions":
			attempt := chatAttempts.Add(1)
			raw, _ := io.ReadAll(r.Body)
			testutil.CheckFalsef(t, len(raw) == 0 || !strings.Contains(string(raw), `"content":"retry body"`), "attempt %d body=%q, want the complete POST body", attempt, raw)
			if attempt == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"expired"}}`)
				return
			}
			testutil.CheckEqual(t, r.Header.Get("Authorization"), "Bearer workos:access-2")
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{
		apiBase: server.URL,
		control: server.Client(),
		stream:  server.Client(),
		creds: Credentials{
			AccessToken:  "access-1",
			RefreshToken: "refresh-1",
			ExpiresAt:    time.Now().Add(time.Hour),
		},
		account: &store.Account{ClineModelIDs: []string{`{"id":"z-ai/glm-5.3-flash"}`}},
	}
	if err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "z-ai/glm-5.3-flash",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "retry body"}}},
	}, nil, nil); err != nil {
		t.Fatalf("SendRequestWithPayload() error=%v", err)
	}
	testutil.Falsef(t, chatAttempts.Load() != 2 || refreshAttempts.Load() != 1, "chat attempts=%d refresh attempts=%d", chatAttempts.Load(), refreshAttempts.Load())
}

// TestClassifyStatusKeepsAForbiddenOutOfTheCredentialPath guards the other half
// of the failure: a 403 used to be reported as a missing credential, which took
// a healthy account out of rotation and demanded a re-login that could not fix
// an entitlement refusal.
func TestClassifyStatusKeepsAForbiddenOutOfTheCredentialPath(t *testing.T) {
	t.Parallel()

	forbidden := classifyStatus(http.StatusForbidden, []byte(`{"error":{"message":"cline-free/x is only available via Cline product surfaces"}}`))
	testutil.CheckFalse(t, isUnauthorized(forbidden), "403 must not be treated as unauthorized: a refresh cannot repair it")
	testutil.CheckContainAll(t, forbidden.Error(), "status=403", "cline-free/x")

	unauthorized := classifyStatus(http.StatusUnauthorized, []byte(`{"error":{"message":"token expired"}}`))
	testutil.CheckFalse(t, !isUnauthorized(unauthorized), "401 must stay unauthorized so one refresh is attempted")
}
