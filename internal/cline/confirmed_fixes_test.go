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
