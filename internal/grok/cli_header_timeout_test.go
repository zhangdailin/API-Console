package grok

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/util"
)

func headerTimeoutTestClient(t *testing.T, server *httptest.Server, timeout time.Duration) (*CLIClient, *store.Account) {
	t.Helper()
	client := NewCLIClient(&config.Config{GrokCLIBaseURL: server.URL + "/v1"})
	client.httpClient = server.Client()
	client.oauth.httpClient = server.Client()
	client.responseHeaderTimeout = timeout
	account := &store.Account{
		OAuthAccessToken: jwtWithClaims(t, `{"sub":"header-timeout-user"}`),
		OAuthExpiresAt:   time.Now().Add(time.Hour),
	}
	return client, account
}

func TestCLIResponseWaitUsesConfiguredGenerationBudget(t *testing.T) {
	for _, seconds := range []int{0, 180, -1} {
		cfg := &config.Config{FirstTokenTimeoutSeconds: seconds}
		client := NewCLIClient(cfg)
		testutil.Equal(t, client.responseHeaderTimeout, cfg.FirstGenerationTimeout())
	}
}

func TestCLIHeaderWaitHonorsSharedDeadlineAndRecordsPartialPhases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	client, account := headerTimeoutTestClient(t, server, time.Second)
	ctx, cancel := util.WithFirstGenerationTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var samples []map[string]int64
	ctx = util.WithHTTPPhaseObserver(ctx, func(v map[string]int64) { samples = append(samples, v) })
	_, err := client.request(ctx, account, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`), nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost shared deadline cause: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("partial phase samples = %d, want 1", len(samples))
	}
	if _, ok := samples[0]["connection_wait_ms"]; !ok {
		t.Fatal("completed connection wait was lost on timeout")
	}
	if _, ok := samples[0]["response_headers_ms"]; ok {
		t.Fatal("fabricated response headers for timed out request")
	}
}

func TestCLIResponsesHeaderTimeoutSwitchesAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client, account := headerTimeoutTestClient(t, server, 20*time.Millisecond)
	started := time.Now()
	_, err := client.request(context.Background(), account, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`), nil)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "response header timeout"), "err=%v", err)
	testutil.True(t, shouldSwitchGrokAccount(err), "timeout must switch account: %v")
	elapsed := time.Since(started)
	testutil.Falsef(t, elapsed > 120*time.Millisecond, "header timeout took %s", elapsed)
}

func TestCLIResponsesHeaderTimeoutStopsAfterHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, "data: done\n\n")
	}))
	defer server.Close()
	client, account := headerTimeoutTestClient(t, server, 20*time.Millisecond)
	resp, err := client.request(context.Background(), account, http.MethodPost, server.URL+"/v1/responses", []byte(`{}`), nil)
	testutil.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	testutil.NoError(t, err)
	testutil.Equal(t, string(body), "data: done\n\n")
}
