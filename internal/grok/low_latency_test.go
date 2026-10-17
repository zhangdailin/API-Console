package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"sync/atomic"
	"testing"
	"time"
)

func TestLowLatencyBypassesQualityHold(t *testing.T) {
	effort := "high"
	req := &chatwire.Request{ReasoningEffort: &effort}
	h := NewHandler(&config.Config{}, nil)
	if !h.shouldHoldQualityTurn(req, ProviderBuild) {
		t.Fatal("default quality protection lost")
	}
	h.SetConfig(&config.Config{GrokLowLatency: true})
	if h.shouldHoldQualityTurn(req, ProviderBuild) {
		t.Fatal("low latency request still buffered")
	}
}

func TestGrokPhysicalBudgetIncludesAuthenticationReplay(t *testing.T) {
	var inference, refresh atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refresh.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"access_token":"fresh-test-token","refresh_token":"rotated-test-token","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		inference.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	defer server.Close()
	for _, limit := range []int{1, 2} {
		inference.Store(0)
		refresh.Store(0)
		client, acc := headerTimeoutTestClient(t, server, time.Second)
		client.oauth.cfg.GrokCLIOAuthTokenURL = server.URL + "/token"
		acc.OAuthRefreshToken = "test-refresh"
		ctx, budget := upstream.WithAttemptBudget(context.Background(), limit)
		_, err := client.doResponsesAt(ctx, acc, "/responses", map[string]any{"model": "test", "stream": true})
		testutil.CheckError(t, err)
		testutil.Equal(t, inference.Load(), int32(limit))
		testutil.Equal(t, refresh.Load(), int32(limit-1))
		testutil.Equal(t, budget.Used(), int64(limit))
	}
}

func TestQualityHoldTimingSeparatesArrivalFromRelease(t *testing.T) {
	started := time.Now().Add(-time.Second)
	hold := newBuildQualityHold(httptest.NewRecorder(), qualityHoldPolicy{}, nil)
	hold.started = started
	result := chatOutcome{FirstToken: started.Add(200 * time.Millisecond)}
	testutil.NoError(t, hold.writer.Commit())
	hold.recordTiming(&result)
	timing := result.QualityTiming
	testutil.True(t, timing.HeldMS >= 1000)
	testutil.Equal(t, timing.FirstContentMS, int64(200))
	testutil.Equal(t, timing.ReleaseMS, timing.HeldMS)
	testutil.True(t, timing.AddedDelayMS >= 800)
	withheld := newBuildQualityHold(httptest.NewRecorder(), qualityHoldPolicy{}, nil)
	withheld.recordTiming(&result)
	testutil.Equal(t, result.QualityTiming.ReleaseMS, int64(-1))
	testutil.Equal(t, result.QualityTiming.AddedDelayMS, int64(-1))
}

func TestLowLatencyJournalDoesNotClaimBufferedDelivery(t *testing.T) {
	h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
		return &config.Config{GrokCLIBaseURL: base, GrokLowLatency: true}
	}, "jwt-user-healthy")
	serveQualityTurn(t, h, accounts[0])
	rows := journal.byAction("grok_request")
	testutil.Equal(t, len(rows), 1)
	testutil.EqualAny(t, rows[0].Metadata["quality_delivery_mode"], "low_latency")
	_, held := rows[0].Metadata["quality_hold_ms"]
	testutil.False(t, held)
	testutil.Equal(t, len(journal.byAction("grok_quality_check")), 0)
}

func TestCLIStreamingNegotiatesUnbufferedSSE(t *testing.T) {
	seen := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client, account := headerTimeoutTestClient(t, server, time.Second)
	resp, err := client.doResponsesOnceAt(context.Background(), account, "/responses", map[string]any{"model": "grok-test", "stream": true})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	headers := <-seen
	if headers.Get("Accept") != "text/event-stream" || headers.Get("Accept-Encoding") != "identity" {
		t.Fatalf("unexpected headers: accept=%q encoding=%q", headers.Get("Accept"), headers.Get("Accept-Encoding"))
	}
	if headers.Get("X-XAI-Token-Auth") == "" || headers.Get("Authorization") == "" {
		t.Fatal("OAuth identity lost")
	}
}
