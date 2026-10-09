package grok

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/config"
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
			sig:  qualityStreamSignals{HasThinking: true, EncryptedBytes: 400, EncryptedFloor: 256, VisibleTokens: 5, FirstVisible: true, VisibleFlushMS: 150},
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
				FirstVisible: true, VisibleFlushMS: 30,
			},
			want: qualityWithhold,
		},
		{
			name: "large encrypted blob then a fast answer is withheld (fake encrypted dump)",
			sig: qualityStreamSignals{
				HasThinking: true, EncryptedBytes: int64(len(dumpEncrypted)), VisibleTokens: 1,
				FirstVisible: true, VisibleFlushMS: 1800,
			},
			want: qualityWithhold,
		},
		{
			name: "plaintext reasoning dumped in 1ms with an 80% bill is withheld",
			sig: qualityStreamSignals{
				HasThinking: true, HasReasoningDelta: true, ReasoningTokens: 900,
				OutputTokens: 1000, VisibleTokens: 25, FirstVisible: true, VisibleFlushMS: 1,
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
	for attempt := 0; attempt < qualityHoldMaxAttemptsDefault-1; attempt++ {
		testutil.Equal(t, decideQualityRetry(qualityWithhold, attempt, qualityHoldMaxAttemptsDefault, qualityRetryFailOpen), qualityActionRetry)
	}
	testutil.Equal(t, decideQualityRetry(qualityWithhold, qualityHoldMaxAttemptsDefault-1, qualityHoldMaxAttemptsDefault, qualityRetryFailOpen), qualityActionDeliverLast)
	testutil.Equal(t, decideQualityRetry(qualityWithhold, qualityHoldMaxAttemptsDefault-1, qualityHoldMaxAttemptsDefault, qualityRetryFailClosed), qualityActionReject)
	testutil.Equal(t, decideQualityRetry(qualityDeliver, 0, qualityHoldMaxAttemptsDefault, qualityRetryFailOpen), qualityActionDeliver)
	// No routing attempt left: a retry must degrade into deliver-last or reject.
	testutil.Equal(t, boundQualityRetry(qualityActionRetry, false, qualityRetryFailOpen), qualityActionDeliverLast)
	testutil.Equal(t, boundQualityRetry(qualityActionRetry, false, qualityRetryFailClosed), qualityActionReject)
	testutil.Equal(t, boundQualityRetry(qualityActionRetry, true, qualityRetryFailClosed), qualityActionRetry)
}

func TestQualityRequestReplayUnsafe(t *testing.T) {
	testutil.False(t, qualityRequestReplayUnsafe(nil), "nil request must be replay-safe")
	testutil.False(t, qualityRequestReplayUnsafe(&chatwire.Request{}), "a plain request must be replay-safe")
	functionTool := map[string]interface{}{"type": "function", "name": "weather"}
	testutil.False(t, qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]interface{}{functionTool}}), "a client-executed function tool must be replay-safe")
	hosted := map[string]interface{}{"type": "web_search"}
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]interface{}{hosted}}), "a hosted search tool must block replay")
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{WebSearchOptions: map[string]interface{}{"search_context_size": "low"}}), "web_search_options must block replay")
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{MCPServers: []map[string]interface{}{{"url": "https://example.test"}}}), "mcp_servers must block replay")
	remoteShell := map[string]interface{}{"type": "shell", "environment": map[string]interface{}{"type": "container"}}
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]interface{}{remoteShell}}), "a hosted shell must block replay")
	localShell := map[string]interface{}{"type": "shell", "environment": map[string]interface{}{"type": "local"}}
	testutil.False(t, qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]interface{}{localShell}}), "a local shell must stay replay-safe")
	// Unknown tool types default to no replay.
	testutil.False(t, !qualityRequestReplayUnsafe(&chatwire.Request{ResponsesTools: []map[string]interface{}{{"type": "code_execution"}}}), "an unknown hosted tool must block replay")
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
	for i := 0; i < (qualityHoldMaxBytes>>20)+1; i++ {
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
	// The zero value normalizes to fail-closed, matching the upstream helper; the
	// handler's own default is fail-open, asserted below.
	testutil.False(t, policy.failOpen(), "the zero-value policy must normalize to fail-closed")
	got := normalizeQualityHoldPolicy(qualityHoldPolicy{OnExhausted: qualityRetryFailClosed})
	testutil.False(t, got.failOpen(), "fail_closed was not honoured")
	got = normalizeQualityHoldPolicy(qualityHoldPolicy{OnExhausted: qualityRetryFailOpen})
	testutil.False(t, !got.failOpen(), "fail_open was not honoured")
	testutil.False(t, !(&Handler{}).qualityHoldPolicy().failOpen(), "the handler default must fail open so a strange pool still answers")
	enabled := false
	h := &Handler{cfg: &config.Config{QualityHoldEnabled: &enabled, QualityHoldMaxAttempts: 3, QualityHoldOnExhausted: qualityRetryFailClosed}}
	testutil.False(t, h.qualityHoldPolicy().Enabled, "an explicit disable was ignored")
	h.cfg = &config.Config{QualityHoldMaxAttempts: 3, QualityHoldTimeoutMs: 2500}
	policy = h.qualityHoldPolicy()
	testutil.Falsef(t, !policy.Enabled || policy.MaxAttempts != 3 || policy.HoldTimeout != 2500*time.Millisecond, "policy=%+v", policy)
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
		payload, _ := json.Marshal(map[string]interface{}{"type": "response.output_text.delta", "delta": dump})
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
	h.serveNativeChat(ctx, rec, req, spec, sess, nil, true)

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

	body, _ := json.Marshal(map[string]interface{}{
		"model": "grok-4.5", "stream": true, "reasoning_effort": "high",
		"messages":          []map[string]interface{}{{"role": "user", "content": "search the web"}},
		"x_responses_tools": []map[string]interface{}{{"type": "web_search"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	// Fail-open: the held body is delivered because it cannot be replayed.
	testutil.MustContain(t, rec.Body.String(), "status loop")
	testutil.Equal(t, calls, 1)
}

// Fail-closed reports the degradation instead of delivering it.
func TestServeNativeChatFailClosedRejectsWithheldTurn(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
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
	h.cfg = &config.Config{
		GrokCLIBaseURL:         degraded.URL + "/v1",
		QualityHoldMaxAttempts: 1,
		QualityHoldOnExhausted: qualityRetryFailClosed,
	}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = degraded.Client()
	h.cliClient.oauth.httpClient = degraded.Client()

	body := `{"model":"grok-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.MustNotContain(t, rec.Body.String(), "status loop")
	testutil.MustContain(t, rec.Body.String(), "quality_degraded")
}

// A disabled hold keeps the historical behaviour: the dump is streamed through.
func TestServeNativeChatStreamsThroughWhenHoldDisabled(t *testing.T) {
	degraded := degradedUpstream(t)
	defer degraded.Close()
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
	disabled := false
	h.cfg = &config.Config{GrokCLIBaseURL: degraded.URL + "/v1", QualityHoldEnabled: &disabled}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = degraded.Client()
	h.cliClient.oauth.httpClient = degraded.Client()

	body := `{"model":"grok-4.5","stream":true,"reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), "status loop")
}
