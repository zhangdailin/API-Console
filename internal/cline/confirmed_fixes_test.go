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

	"orchids-api/internal/config"
	"orchids-api/internal/prompt"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestChatDoesNotLocallyRetryNonAuthFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":"busy"}`)
	}))
	defer server.Close()
	client := &Client{apiBase: server.URL, stream: server.Client(), creds: Credentials{AccessToken: "token"}, account: &store.Account{ClineModelIDs: []string{`{"id":"model-a"}`}}}
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "model-a", RequestID: "logical-1", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	testutil.False(t, err == nil, "expected upstream error")
	testutil.Equal(t, attempts.Load(), 1)
}

func TestChatKeepsLogicalTaskIDAcross401Refresh(t *testing.T) {
	var attempts atomic.Int32
	var taskIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/refresh":
			_, _ = io.WriteString(w, `{"data":{"accessToken":"new","refreshToken":"refresh","expiresAt":"2099-01-01T00:00:00Z"}}`)
		case "/chat/completions":
			taskIDs = append(taskIDs, r.Header.Get("X-Task-ID"))
			if attempts.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
	}))
	defer server.Close()
	client := &Client{apiBase: server.URL, stream: server.Client(), control: server.Client(), creds: Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour)}, account: &store.Account{ClineModelIDs: []string{`{"id":"model-a"}`}}}
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "model-a", RequestID: "logical-1", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	testutil.NoError(t, err)
	testutil.Falsef(t, len(taskIDs) != 2 || taskIDs[0] != "sess_logical-1" || taskIDs[1] != taskIDs[0], "task ids=%v", taskIDs)
}

func TestResolveModelEnforcesAccountCatalog(t *testing.T) {
	client := &Client{account: &store.Account{ClineModelIDs: CatalogSnapshot([]Model{{ID: "allowed"}})}}
	_, err := client.resolveModel(upstream.UpstreamRequest{Model: "denied"})
	testutil.Error(t, err)
	got, err := client.resolveModel(upstream.UpstreamRequest{Model: "ALLOWED"})
	testutil.Falsef(t, err != nil || got != "allowed", "resolve allowed=%q err=%v", got, err)
}

func TestConsumeStreamPreservesFinishReasonAndReasoningUsage(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":4}}}` + "\n\n"
	result, err := consumeStream(strings.NewReader(body), false, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, result.FinishReason(), "max_tokens")
	testutil.Equal(t, result.Usage["reasoningTokens"], 4)
	testutil.Equal(t, result.Usage["reasoning_tokens"], 4)
}

// A request without an id mints one; the body and the header must carry the
// same one, or the upstream sees two sessions for a single turn.
func TestChatSessionIDMatchesTaskHeaderWithoutRequestID(t *testing.T) {
	var header, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		header, body = r.Header.Get("X-Task-ID"), string(raw)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	client := &Client{apiBase: server.URL, stream: server.Client(), creds: Credentials{AccessToken: "token"}, account: &store.Account{ClineModelIDs: []string{`{"id":"model-a"}`}}}
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "hi"}}}}, nil, nil)
	testutil.NoError(t, err, "SendRequestWithPayload() error = %v")
	testutil.True(t, strings.HasPrefix(header, "sess_"), "X-Task-ID = %q", header)
	testutil.MustContain(t, body, `"session_id":"`+header+`"`)
}

// Verification reads the catalog once: that read is both the snapshot and the
// liveness proof.
func TestVerifyAccountReadsCatalogOnce(t *testing.T) {
	var catalogReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/recommended-models") {
			catalogReads.Add(1)
			_, _ = io.WriteString(w, `{"free":[{"id":"vendor/model-a","name":"Model A"}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"No plan history found for user"}`)
	}))
	defer server.Close()
	acc := &store.Account{ID: 1, ClineAccessToken: "token", ClineRefreshToken: "refresh"}
	_, status, err := VerifyAccount(context.Background(), acc, &config.Config{ClineAPIBaseURL: server.URL}, nil)
	testutil.NoError(t, err, "VerifyAccount() error = %v")
	testutil.Equal(t, status, 0)
	testutil.Equal(t, catalogReads.Load(), 1)
	testutil.Equal(t, len(acc.ClineModelIDs), 1)
	testutil.Equal(t, acc.ClinePlan, "free")
}
