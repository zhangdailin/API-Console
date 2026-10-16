package grok

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestClassifyQualityHold(t *testing.T) {
	dumpEncrypted := strings.Repeat("a", int(qualityMinEncryptedChars)+64)
	cases := []struct {
		name string
		sig  qualityStreamSignals
		want qualityVerdict
	}{
		{
			name: "plaintext reasoning releases immediately",
			sig:  qualityStreamSignals{HasThinking: true, HasReasoningDelta: true, ReasoningTokens: 10, VisibleTokens: 3},
			want: qualityDeliver,
		},
		{
			name: "cipher-only thinking with no answer yet waits",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256},
			want: qualityWait,
		},
		{
			// The upstream deliberately does not gate this on visible size: the
			// 18190/18183 dumps answered in under two seconds and leaked through
			// the minimum-output check.
			name: "cipher blob then a fast answer is withheld even when short",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 5, FirstVisible: true, VisibleFlushMS: 150, Terminal: true},
			want: qualityWithhold,
		},
		{
			name: "cipher-only thinking delivers after two seconds of visible text",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 100, FirstVisible: true, VisibleFlushMS: 2500},
			want: qualityDeliver,
		},
		{
			// Cipher-only thinking with a zero reasoning bill and a large visible
			// answer is the status-loop drool, terminal event or not.
			name: "cipher-only thinking with zero reasoning tokens is withheld at the terminal event",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 200, Terminal: true},
			want: qualityWithhold,
		},
		{
			// A healthy encrypted-thinking stream bills reasoning tokens, so the
			// drool detector steps aside and the terminal event releases it.
			name: "cipher-only thinking with a reasoning bill delivers at the terminal event",
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, ReasoningTokens: 300, VisibleTokens: 200, Terminal: true},
			want: qualityDeliver,
		},
		{
			name: "no reasoning at all with enough visible text is withheld at the end",
			sig:  qualityStreamSignals{VisibleTokens: 75, Terminal: true},
			want: qualityWithhold,
		},
		{
			name: "reasoning stub then a short dump is withheld (burst)",
			sig: qualityStreamSignals{
				ReasoningStarted: true, ReasoningTokens: 954, VisibleTokens: 1,
				FirstVisible: true, VisibleFlushMS: 30, Terminal: true,
			},
			want: qualityWithhold,
		},
		{
			name: "large encrypted blob then a fast answer is withheld (fake encrypted dump)",
			sig: qualityStreamSignals{
				HasThinking: true, EncryptedBytes: int64(len(dumpEncrypted)), VisibleTokens: 1,
				FirstVisible: true, VisibleFlushMS: 1800, Terminal: true,
			},
			want: qualityWithhold,
		},
		{
			name: "plaintext reasoning dumped in 1ms with an 80% bill is withheld",
			sig: qualityStreamSignals{
				HasThinking: true, HasReasoningDelta: true, ReasoningTokens: 900,
				OutputTokens: 1000, VisibleTokens: 25, FirstVisible: true, VisibleFlushMS: 1, Terminal: true,
			},
			want: qualityWithhold,
		},
		{
			name: "cipher drool (status loop) is withheld once visible text is large",
			sig: qualityStreamSignals{
				HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256,
				VisibleTokens: qualityCipherDroolVisible + 1,
			},
			want: qualityWithhold,
		},
		{
			name: "empty turn keeps waiting",
			sig:  qualityStreamSignals{},
			want: qualityWait,
		},
		{
			name: "expired hold with nothing visible keeps waiting rather than releasing junk",
			sig:  qualityStreamSignals{HoldExpired: true},
			want: qualityWait,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Equal(t, classifyQualityHold(tc.sig, qualityHoldMinOutputDefault), tc.want)
		})
	}
}

func TestDecideQualityRetryPolicy(t *testing.T) {
	// Six attempts: the first five withhold-and-retry, the sixth delivers.
	for attempt := range qualityHoldMaxAttemptsDefault - 1 {
		testutil.Equal(t, decideQualityRetry(qualityWithhold, attempt, qualityHoldMaxAttemptsDefault), qualityActionRetry)
	}
	testutil.Equal(t, decideQualityRetry(qualityWithhold, qualityHoldMaxAttemptsDefault-1, qualityHoldMaxAttemptsDefault), qualityActionDeliverLast)
	testutil.Equal(t, decideQualityRetry(qualityDeliver, 0, qualityHoldMaxAttemptsDefault), qualityActionDeliver)
	// No routing attempt left: deliver the last response the upstream produced.
	testutil.Equal(t, boundQualityRetry(qualityActionRetry, false), qualityActionDeliverLast)
	testutil.Equal(t, boundQualityRetry(qualityActionRetry, true), qualityActionRetry)
}

func TestQualityRequestReplayUnsafe(t *testing.T) {
	testutil.False(t, qualityRequestReplayUnsafe(nil), "nil request must be replay-safe")
	testutil.False(t, qualityRequestReplayUnsafe(&chatwire.Request{}), "a plain request must be replay-safe")
	functionTool := map[string]any{"type": "function", "name": "weather"}
	testutil.False(t, qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]any{functionTool}}), "a client-executed function tool must be replay-safe")
	hosted := map[string]any{"type": "web_search"}
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]any{hosted}}), "a hosted search tool must block replay")
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{WebSearchOptions: map[string]any{"search_context_size": "low"}}), "web_search_options must block replay")
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{MCPServers: []map[string]any{{"url": "https://example.test"}}}), "mcp_servers must block replay")
	remoteShell := map[string]any{"type": "shell", "environment": map[string]any{"type": "container"}}
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]any{remoteShell}}), "a hosted shell must block replay")
	localShell := map[string]any{"type": "shell", "environment": map[string]any{"type": "local"}}
	testutil.False(t, qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]any{localShell}}), "a local shell must stay replay-safe")
	// Unknown tool types default to no replay.
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]any{{"type": "code_execution"}}}), "an unknown hosted tool must block replay")
}

func TestDeferredResponseWriterHoldsThenCommits(t *testing.T) {
	rec := httptest.NewRecorder()
	deferred := newDeferredResponseWriter(rec)
	deferred.Header().Set("Content-Type", "text/event-stream")
	deferred.WriteHeader(http.StatusOK)
	_, err := io.WriteString(deferred, "data: first\n\n")
	testutil.CheckNoError(t, err)
	deferred.Flush()

	// Nothing may reach the client while the response is held.
	testutil.Falsef(t, rec.Body.Len() != 0 || rec.Code != http.StatusOK, "held response leaked: code=%d body=%q", rec.Code, rec.Body.String())
	testutil.NotEqual(t, deferred.Buffered(), 0)
	testutil.NoError(t, deferred.Commit(), "commit: %v")
	testutil.Equal(t, rec.Body.String(), "data: first\n\n")
	testutil.Equal(t, rec.Header().Get("Content-Type"), "text/event-stream")
	// After the commit the writer is transparent: no second status line.
	_, err = io.WriteString(deferred, "data: second\n\n")
	testutil.CheckNoError(t, err)
	testutil.Equal(t, rec.Body.String(), "data: first\n\ndata: second\n\n")
}

func TestDeferredResponseWriterParksWithoutRevealing(t *testing.T) {
	rec := httptest.NewRecorder()
	deferred := newDeferredResponseWriter(rec)
	deferred.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(deferred, "degraded dump")

	parked := deferred.Park()
	testutil.Falsef(t, parked == nil || string(parked.body) != "degraded dump", "parked=%+v", parked)
	// A parked response stays invisible and later writes are swallowed.
	_, err := io.WriteString(deferred, "more")
	testutil.CheckNoError(t, err)
	testutil.Equal(t, rec.Body.Len(), 0)
	// Fail-open delivery writes the parked body to the real client.
	testutil.NoError(t, parked.CommitTo(rec), "commit parked: %v")
	testutil.Equal(t, rec.Body.String(), "degraded dump")
}

func TestDeferredResponseWriterOverflowDelivers(t *testing.T) {
	rec := httptest.NewRecorder()
	deferred := newDeferredResponseWriter(rec)
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for range (qualityHoldMaxBytes >> 20) + 1 {
		_, err := deferred.Write(chunk)
		testutil.CheckNoError(t, err)
	}
	// Past the cap the response is delivered instead of buffered without bound.
	testutil.NotEqual(t, rec.Body.Len(), 0)
}

func TestQualityHoldPolicyDefaults(t *testing.T) {
	policy := normalizeQualityHoldPolicy(qualityHoldPolicy{})
	testutil.Equal(t, policy.MaxAttempts, qualityHoldMaxAttemptsDefault)
	testutil.Equal(t, policy.HoldTimeout, qualityHoldTimeoutDefault)
	policy = normalizeQualityHoldPolicy(qualityHoldPolicy{MaxAttempts: 3, HoldTimeout: 2500 * time.Millisecond})
	testutil.Falsef(t, policy.MaxAttempts != 3 || policy.HoldTimeout != 2500*time.Millisecond, "policy=%+v", policy)
}

// degradedUpstream replays the cipher-drool dump: an encrypted stub with zero
// reasoning tokens, then the whole visible answer at once.
func degradedUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	encrypted := strings.Repeat("ZmFrZS1jaXBoZXI", 40)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\""+encrypted+"\"}}\n\n")
		dump := strings.Repeat("status loop ", 120)
		payload, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "delta": dump})
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: "+string(payload)+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_degraded\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":1440,\"output_tokens_details\":{\"reasoning_tokens\":0}}}}\n\n")
	}))
}

// healthyUpstream streams plaintext reasoning before the answer.
func healthyUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"weighing the request\"}\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"healthy answer\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_healthy\",\"status\":\"completed\",\"output\":[{\"id\":\"rs_1\",\"type\":\"reasoning\"}],\"usage\":{\"input_tokens\":10,\"output_tokens\":12,\"output_tokens_details\":{\"reasoning_tokens\":8}}}}\n\n")
	}))
}

// TestServeNativeChatWithholdsDegradedTurnAndRetriesAnotherAccount is the
// end-to-end proof for A6-1: a degraded stream never reaches the client, the
// credential is penalised, and the answer comes from the next account.
//
// The first attempt is pinned to the degraded credential by account id, because
// which account the balancer picks first is not what this test is about: the hold
// and the retry are.
func TestServeNativeChatWithholdsDegradedTurnAndRetriesAnotherAccount(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	healthy := healthyUpstream(t)
	defer healthy.Close()

	var healthyCalls int
	var degradedCalls int
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Route by the credential the request carries.
		if strings.Contains(r.Header.Get("Authorization"), "jwt-user-healthy") {
			healthyCalls++
			healthy.Config.Handler.ServeHTTP(w, r)
			return
		}
		degradedCalls++
		degraded.Config.Handler.ServeHTTP(w, r)
	}))
	defer routing.Close()

	h, s, _ := setupValidationHandler(t)
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	degradedAcc := &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-degraded", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}
	healthyAcc := &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-healthy", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}
	for _, acc := range []*store.Account{degradedAcc, healthyAcc} {
		testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount: %v")
	}

	h.cfg = &config.Config{GrokCLIBaseURL: routing.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	sess, err := h.openCLIAccountSessionByID(ctx, degradedAcc.ID, "grok-4.5")
	testutil.NoError(t, err, "openCLIAccountSessionByID: %v")
	defer sess.Close()
	spec, ok := h.resolveConversationModel(ctx, "grok-4.5")
	testutil.True(t, ok, "grok-4.5 did not resolve")
	effort := "high"
	req := &chatwire.Request{
		Model: "grok-4.5", Stream: true, ReasoningEffort: &effort,
		Messages: []chatwire.Message{{Role: "user", Content: "hello"}},
	}
	rec := httptest.NewRecorder()
	h.serveNativeChat(ctx, rec, req, spec, sess, nil)

	testutil.Equal(t, rec.Code, http.StatusOK)
	stream := rec.Body.String()
	testutil.MustNotContain(t, stream, "status loop")
	testutil.MustContain(t, stream, "healthy answer")
	testutil.NotEqual(t, healthyCalls, 0)
	testutil.Equal(t, degradedCalls, 1)

	// The degraded credential is parked so it stops serving dumps.
	stored, err := s.ListAccounts(ctx)
	testutil.NoError(t, err, "ListAccounts: %v")
	parked := 0
	for _, acc := range stored {
		if acc.QualityFailures > 0 {
			parked++
			testutil.Falsef(t, acc.QualityCooldownUntil.IsZero() || !acc.QualityCooldownUntil.After(time.Now()), "account %d was counted but not cooled: %v", acc.ID, acc.QualityCooldownUntil)
		}
	}
	testutil.Equal(t, parked, 1)
}

// The HTTP entry point never hands a degraded dump to a client either, whichever
// account the balancer happens to pick first.
func TestHandleChatCompletionsNeverDeliversDegradedDump(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	healthy := healthyUpstream(t)
	defer healthy.Close()
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "jwt-user-healthy") {
			healthy.Config.Handler.ServeHTTP(w, r)
			return
		}
		degraded.Config.Handler.ServeHTTP(w, r)
	}))
	defer routing.Close()

	h, s, _ := setupValidationHandler(t)
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	for _, token := range []string{"jwt-user-degraded", "jwt-user-healthy"} {
		if err := s.CreateAccount(ctx, &store.Account{
			AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
			OAuthAccessToken: token, GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
		}); err != nil {
			t.Fatalf("CreateAccount: %v", err)
		}
	}
	h.cfg = &config.Config{GrokCLIBaseURL: routing.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	body := `{"model":"grok-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	stream := rec.Body.String()
	testutil.MustNotContain(t, stream, "status loop")
	testutil.MustContain(t, stream, "healthy answer")
}

// A request whose tools have already produced an external side effect is still
// judged and penalised, but it is never replayed on another account.
func TestServeNativeChatDoesNotReplayHostedToolTurn(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
	calls := 0
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		degraded.Config.Handler.ServeHTTP(w, r)
	}))
	defer routing.Close()

	h, s, _ := setupValidationHandler(t)
	ctx := context.Background()
	if err := s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5",
		Status: store.ModelStatusAvailable, Verified: true,
	}); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	if err := s.CreateAccount(ctx, &store.Account{
		AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
		OAuthAccessToken: "jwt-user-degraded", GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: routing.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	body, _ := json.Marshal(map[string]any{
		"model": "grok-4.5", "stream": true, "reasoning_effort": "high",
		"messages":          []map[string]any{{"role": "user", "content": "search the web"}},
		"x_responses_tools": []map[string]any{{"type": "web_search"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	// Fail-open: the held body is delivered because it cannot be replayed.
	testutil.MustContain(t, rec.Body.String(), "status loop")
	testutil.Equal(t, calls, 1)
}

func TestNativeChatTransportAndQualityRetryShareBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		responses  []string
		budget     int
		unsafe     bool
		wantCalls  int32
		wantStatus int
		wantAnswer string
	}{
		{"timeout switches account", []string{"timeout", "healthy"}, 3, false, 2, 200, "healthy answer"},
		{"transport then quality then success", []string{"503", "degraded", "healthy"}, 3, false, 3, 200, "healthy answer"},
		{"mixed failures exhaust total budget", []string{"503", "degraded", "503", "healthy"}, 3, false, 3, 502, ""},
		{"one attempt bounds quality too", []string{"degraded", "healthy"}, 1, false, 1, 200, "status loop"},
		{"hosted tool cannot replay", []string{"timeout", "healthy"}, 3, true, 1, 504, ""},
		{"visible partial output cannot replay", []string{"partial", "healthy"}, 3, false, 1, 200, "partial answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			healthy := healthyUpstream(t)
			defer healthy.Close()
			degraded := degradedUpstream(t)
			defer degraded.Close()
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := int(calls.Add(1)) - 1
				if n >= len(tc.responses) {
					w.WriteHeader(500)
					return
				}
				switch tc.responses[n] {
				case "timeout":
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case <-r.Context().Done():
					case <-time.After(time.Second):
					}
				case "503":
					w.WriteHeader(503)
				case "degraded":
					degraded.Config.Handler.ServeHTTP(w, r)
				case "healthy":
					healthy.Config.Handler.ServeHTTP(w, r)
				case "partial":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thinking\"}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial answer\"}\n\n")
				}
			}))
			defer upstream.Close()
			h, database, _ := setupValidationHandler(t)
			ctx := context.Background()
			var first *store.Account
			for i := range 4 {
				acc := &store.Account{AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
					OAuthAccessToken: fmt.Sprintf("jwt-user-%d", i), GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now()}
				testutil.NoError(t, database.CreateAccount(ctx, acc))
				if first == nil {
					first = acc
				}
			}
			h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1", AccountSwitchCount: tc.budget}
			h.cliClient = NewCLIClient(h.cfg)
			h.cliClient.SetAccountStore(database)
			h.cliClient.httpClient = upstream.Client()
			h.cliClient.oauth.httpClient = upstream.Client()
			h.cliClient.responseHeaderTimeout = 50 * time.Millisecond
			sess, err := h.openCLIAccountSessionByID(ctx, first.ID, "grok-4.5")
			testutil.NoError(t, err)
			defer sess.Close()
			spec, _, ok := ResolveModelAlias("grok-4.5", []modelcatalog.Profile{{ModelID: "grok-4.5"}})
			testutil.True(t, ok)
			effort := "high"
			req := &chatwire.Request{Model: "grok-4.5", Stream: true, ReasoningEffort: &effort, Messages: []chatwire.Message{{Role: "user", Content: "hello"}}}
			if tc.unsafe {
				req.WebSearchOptions = map[string]any{}
			}
			rec := httptest.NewRecorder()
			h.serveNativeChat(ctx, rec, req, spec, sess, nil)
			sess.Close()
			testutil.Equal(t, calls.Load(), tc.wantCalls)
			testutil.Equal(t, rec.Code, tc.wantStatus)
			if tc.wantAnswer != "" {
				testutil.MustContain(t, rec.Body.String(), tc.wantAnswer)
			}
			testutil.Equal(t, h.connTracker.GetCount(first.ID), int64(0))
		})
	}
}

func TestAccountRetryStopsOnSharedCancellation(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	h := &Handler{cfg: &config.Config{AccountSwitchCount: 3}}
	sess := &chatAccountSession{acc: &store.Account{ID: 1}}
	var attempts, switches int
	_, err := h.retryWithAccountSwitch(ctx, sess, 0, func() (*http.Response, error) {
		attempts++
		cancel(context.DeadlineExceeded)
		return nil, errCLIResponseHeaderTimeout
	}, func([]int64) (*chatAccountSession, error) {
		switches++
		return nil, fmt.Errorf("must not switch")
	}, nil)
	testutil.Equal(t, err, context.DeadlineExceeded)
	testutil.Equal(t, attempts, 1)
	testutil.Equal(t, switches, 0)
}

// classifyQualityHold exposes just the verdict for table-driven policy tests.
func classifyQualityHold(sig qualityStreamSignals, minOutput int64) qualityVerdict {
	verdict, _ := classifyQualityHoldReason(sig, minOutput)
	return verdict
}
