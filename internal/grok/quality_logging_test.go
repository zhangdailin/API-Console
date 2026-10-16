package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/config"
	"orchids-api/internal/modelcatalog"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// recordingAuditLogger keeps every journal event a handler writes.
type recordingAuditLogger struct {
	mu     sync.Mutex
	events []audit.Event
}

func (l *recordingAuditLogger) Log(_ context.Context, event audit.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *recordingAuditLogger) byAction(action string) []audit.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []audit.Event
	for _, event := range l.events {
		if event.Action == action {
			out = append(out, event)
		}
	}
	return out
}

// dump renders everything that was journalled, the way the journal stores it.
func (l *recordingAuditLogger) dump(t *testing.T) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	raw, err := json.Marshal(l.events)
	testutil.NoError(t, err, "marshal journal: %v")
	return string(raw)
}

var knownQualityReasons = map[string]bool{
	qualityReasonBurstDump: true, qualityReasonCipherDrool: true, qualityReasonFakeEncryptedDump: true,
	qualityReasonFastReasoningRatio: true, qualityReasonNoReasoning: true,
}

// qualityLoggingHandler builds a handler whose upstream answers by credential:
// the "degraded" token gets the dump, every other token the healthy stream.
func qualityLoggingHandler(t *testing.T, cfg func(base string) *config.Config, tokens ...string) (*Handler, *recordingAuditLogger, []*store.Account) {
	t.Helper()
	degraded := degradedUpstream(t)
	t.Cleanup(degraded.Close)
	healthy := healthyUpstream(t)
	t.Cleanup(healthy.Close)
	routing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), "degraded") {
			degraded.Config.Handler.ServeHTTP(w, r)
			return
		}
		healthy.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(routing.Close)

	h, s, _ := setupValidationHandler(t)
	ctx := context.Background()
	testutil.NoError(t, s.CreateModel(ctx, &store.Model{
		Channel: "Grok", ModelID: "grok-4.5", Name: "Grok 4.5", Status: store.ModelStatusAvailable, Verified: true,
	}), "CreateModel: %v")
	accounts := make([]*store.Account, 0, len(tokens))
	for _, token := range tokens {
		acc := &store.Account{
			AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, Weight: 1,
			OAuthAccessToken: token, GrokModels: []string{"grok-4.5"}, GrokModelsSyncedAt: time.Now(),
		}
		testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount: %v")
		accounts = append(accounts, acc)
	}

	h.cfg = cfg(routing.URL + "/v1")
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.SetAccountStore(s)
	h.cliClient.httpClient = routing.Client()
	h.cliClient.oauth.httpClient = routing.Client()

	journal := &recordingAuditLogger{}
	h.SetAuditLogger(journal)
	return h, journal, accounts
}

func serveQualityTurn(t *testing.T, h *Handler, first *store.Account) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	sess, err := h.openCLIAccountSessionByID(ctx, first.ID, "grok-4.5")
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
	return rec
}

// TestQualityHoldJournalsAWithheldTurnAndItsRecovery is the record an operator
// reads to answer "why did this request take two accounts": which detector
// refused the turn, what happened to the credential, and that the client was
// served by the next account rather than by the dump.
func TestQualityHoldJournalsAWithheldTurnAndItsRecovery(t *testing.T) {
	h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
		return &config.Config{GrokCLIBaseURL: base}
	}, "jwt-user-degraded", "jwt-user-healthy")
	degradedAcc := accounts[0]

	rec := serveQualityTurn(t, h, degradedAcc)
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), "healthy answer")

	withheld := journal.byAction("grok_quality_degraded")
	testutil.Equal(t, len(withheld), 1)
	row := withheld[0]
	testutil.Equal(t, row.Kind, audit.KindRequest)
	testutil.Equal(t, row.AccountID, degradedAcc.ID)
	testutil.Equal(t, row.Attempt, 1)
	testutil.Equal(t, row.Status, "degraded")
	testutil.Equal(t, row.Model, "grok-4.5")
	testutil.Falsef(t, !knownQualityReasons[row.Details], "withheld row names no known detector: %q", row.Details)
	testutil.EqualAny(t, row.Metadata["reason"], row.Details)
	testutil.EqualAny(t, row.Metadata["mode"], "stream")
	testutil.EqualAny(t, row.Metadata["guard"], string(qualityGuardParked))

	// The penalty is a system event of its own, tied to the same account, so
	// "why is this account parked" does not depend on the server log.
	guard := journal.byAction("grok_quality_guard")
	testutil.Equal(t, len(guard), 1)
	testutil.Equal(t, guard[0].Kind, audit.KindSystem)
	testutil.Equal(t, guard[0].AccountID, degradedAcc.ID)
	testutil.Equal(t, guard[0].Status, string(qualityGuardParked))
	testutil.EqualAny(t, guard[0].Metadata["failures"], 1)
	testutil.EqualAny(t, guard[0].Metadata["withheld"], true)
	testutil.True(t, guard[0].Metadata["cooldown_until"] != nil, "a parked credential must record when it comes back")

	resolution := journal.byAction("grok_quality_resolution")
	testutil.Equal(t, len(resolution), 1)
	testutil.Equal(t, resolution[0].Status, qualityResolutionRetried)
	testutil.Equal(t, resolution[0].Attempt, 1)
	testutil.EqualAny(t, resolution[0].Metadata["client_received_degraded"], false)
	testutil.NotEqual(t, resolution[0].AccountID, degradedAcc.ID)

	// The withheld attempt's own request row must not read as a delivered answer.
	var withheldRequests, deliveredRequests int
	for _, event := range journal.byAction("grok_request") {
		if event.Metadata["quality_withheld"] == true {
			withheldRequests++
			testutil.Equal(t, event.Status, "quality_degraded")
			testutil.Equal(t, event.AccountID, degradedAcc.ID)
			testutil.EqualAny(t, event.Metadata["quality_reason"], row.Details)
			continue
		}
		deliveredRequests++
		testutil.NotEqual(t, event.Status, "quality_degraded")
	}
	testutil.Equal(t, withheldRequests, 1)
	testutil.Equal(t, deliveredRequests, 1)
	checks := journal.byAction("grok_quality_check")
	testutil.Equal(t, len(checks), 3)
	testutil.Equal(t, checks[0].Status, "checking")
	testutil.Equal(t, checks[1].Status, "checking")
	testutil.Equal(t, checks[2].Status, "passed")
	retries := journal.byAction("grok_quality_retry")
	testutil.Equal(t, len(retries), 1)
	testutil.Equal(t, retries[0].Attempt, 2)
	testutil.EqualAny(t, retries[0].Metadata["from_account_id"], degradedAcc.ID)
	testutil.EqualAny(t, retries[0].Metadata["to_account_id"], retries[0].AccountID)

	// The journal describes the turn; it must not contain it, nor a credential.
	dump := journal.dump(t)
	testutil.MustNotContainAny(t, dump, "status loop", "healthy answer", "jwt-user-degraded", "jwt-user-healthy", "ZmFrZS1jaXBoZXI")
}

// TestQualityRetryResolutionJournal checks the fixed fail-open policy.
func TestQualityRetryResolutionJournal(t *testing.T) {
	h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
		return &config.Config{GrokCLIBaseURL: base}
	}, "jwt-user-degraded")
	rec := serveQualityTurn(t, h, accounts[0])
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), "status loop")
	resolution := journal.byAction("grok_quality_resolution")
	testutil.Equal(t, len(resolution), 1)
	testutil.Equal(t, resolution[0].Status, qualityResolutionDeliveredDegraded)
	testutil.Equal(t, resolution[0].AccountID, accounts[0].ID)
	testutil.EqualAny(t, resolution[0].Metadata["client_received_degraded"], true)
	testutil.EqualAny(t, resolution[0].Metadata["on_exhausted"], qualityRetryFailOpen)
}

func TestHealthyTurnJournalsQualityCheckProcess(t *testing.T) {
	h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
		return &config.Config{GrokCLIBaseURL: base}
	}, "jwt-user-healthy")

	rec := serveQualityTurn(t, h, accounts[0])
	testutil.Equal(t, rec.Code, http.StatusOK)
	for _, action := range []string{"grok_quality_degraded", "grok_quality_guard", "grok_quality_resolution"} {
		testutil.Equal(t, len(journal.byAction(action)), 0)
	}
	requests := journal.byAction("grok_request")
	testutil.Equal(t, len(requests), 1)
	testutil.True(t, requests[0].Metadata["quality_withheld"] == nil, "a delivered turn must not be marked withheld")
	checks := journal.byAction("grok_quality_check")
	testutil.Equal(t, len(checks), 2)
	testutil.Equal(t, checks[0].Status, "checking")
	testutil.Equal(t, checks[1].Status, "passed")
	testutil.EqualAny(t, requests[0].Metadata["quality_check_status"], "passed")
	testutil.EqualAny(t, requests[0].Metadata["quality_checked_attempts"], 1)
	testutil.MustNotContainAny(t, journal.dump(t), "healthy answer", "jwt-user-healthy")
}

func TestCollectedTurnJournalsQualityCheckProcess(t *testing.T) {
	for _, tc := range []struct {
		name, body, verdict string
	}{
		{"healthy", `{"status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"considering the request"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"healthy answer"}]}],"usage":{"input_tokens":10,"output_tokens":12,"output_tokens_details":{"reasoning_tokens":8}}}`, "passed"},
		{"interrupted", `{"output":`, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
				return &config.Config{GrokCLIBaseURL: base}
			}, "jwt-user-healthy")
			effort := "high"
			req := &chatwire.Request{Model: "grok-4.5", ReasoningEffort: &effort}
			response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
			rec := httptest.NewRecorder()
			h.finishUpstreamChat(context.Background(), rec, req, &chatAccountSession{acc: accounts[0]}, nil, "build", "/responses", func() http.Header { return http.Header{} }, nil, response, nil, 1)
			checks := journal.byAction("grok_quality_check")
			testutil.Equal(t, len(checks), 2)
			testutil.Equal(t, checks[0].Status, "checking")
			testutil.Equal(t, checks[1].Status, tc.verdict)
			testutil.EqualAny(t, checks[1].Metadata["mode"], "collected")
			testutil.MustNotContainAny(t, journal.dump(t), "healthy answer", "considering the request", "jwt-user-healthy")
		})
	}
}

func TestQualityHoldUsesAccountDefaultReasoning(t *testing.T) {
	for _, streaming := range []bool{true, false} {
		for _, requested := range []string{"omitted", "", "none"} {
			t.Run(fmt.Sprintf("stream=%t/effort=%s", streaming, requested), func(t *testing.T) {
				h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
					return &config.Config{GrokCLIBaseURL: base}
				}, "jwt-user-healthy")
				if !streaming {
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						w.Header().Set("Content-Type", "application/json")
						io.WriteString(w, `{"id":"resp_healthy","status":"completed","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"considering the request"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"healthy answer"}]}],"usage":{"input_tokens":10,"output_tokens":12,"output_tokens_details":{"reasoning_tokens":8}}}`)
					}))
					t.Cleanup(upstream.Close)
					h.SetConfig(&config.Config{GrokCLIBaseURL: upstream.URL + "/v1"})
				}
				acc := accounts[0]
				acc.GrokModelCatalog = []modelcatalog.Profile{{
					ModelID: "grok-4.5", SupportsReasoningEffort: true,
					ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high",
				}}
				testutil.NoError(t, h.lb.Store.UpdateAccount(context.Background(), acc), "UpdateAccount: %v")
				req := &chatwire.Request{Model: "grok-4.5", Stream: streaming, Messages: []chatwire.Message{{Role: "user", Content: "hello"}}}
				if requested != "omitted" {
					req.ReasoningEffort = &requested
				}
				sess, err := h.openCLIAccountSessionByID(context.Background(), acc.ID, req.Model)
				testutil.NoError(t, err, "open account: %v")
				defer sess.Close()
				spec, ok := h.resolveConversationModel(context.Background(), req.Model)
				testutil.True(t, ok, "model did not resolve")
				rec := httptest.NewRecorder()
				h.serveNativeChat(context.Background(), rec, req, spec, sess, nil)
				testutil.Equal(t, rec.Code, http.StatusOK)
				checks := journal.byAction("grok_quality_check")
				if requested == "none" {
					testutil.Equal(t, len(checks), 0)
				} else {
					testutil.Equal(t, len(checks), 2)
					testutil.Equal(t, checks[0].Status, "checking")
					testutil.Equal(t, checks[1].Status, "passed")
				}
				if requested == "omitted" {
					testutil.True(t, req.ReasoningEffort == nil, "account default must not mutate client input")
				} else {
					testutil.Equal(t, *req.ReasoningEffort, requested)
				}
			})
		}
	}
}

func TestQualityHoldDefaultReasoningWithholdsAndRetries(t *testing.T) {
	h, journal, accounts := qualityLoggingHandler(t, func(base string) *config.Config {
		return &config.Config{GrokCLIBaseURL: base}
	}, "jwt-user-degraded", "jwt-user-healthy")
	for i, acc := range accounts {
		effort := []string{"high", "low"}[i]
		acc.GrokModelCatalog = []modelcatalog.Profile{{
			ModelID: "grok-4.5", SupportsReasoningEffort: true,
			ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: effort,
		}}
		testutil.NoError(t, h.lb.Store.UpdateAccount(context.Background(), acc), "UpdateAccount: %v")
	}
	req := &chatwire.Request{Model: "grok-4.5", Stream: true, Messages: []chatwire.Message{{Role: "user", Content: "hello"}}}
	sess, err := h.openCLIAccountSessionByID(context.Background(), accounts[0].ID, req.Model)
	testutil.NoError(t, err, "open account: %v")
	defer sess.Close()
	spec, ok := h.resolveConversationModel(context.Background(), req.Model)
	testutil.True(t, ok, "model did not resolve")
	rec := httptest.NewRecorder()
	h.serveNativeChat(context.Background(), rec, req, spec, sess, nil)
	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), "healthy answer")
	testutil.MustNotContain(t, rec.Body.String(), "status loop")
	testutil.True(t, req.ReasoningEffort == nil, "retry must not reuse the first account's default")
	testutil.Equal(t, len(journal.byAction("grok_quality_degraded")), 1)
	testutil.Equal(t, len(journal.byAction("grok_quality_retry")), 1)
	resolution := journal.byAction("grok_quality_resolution")
	testutil.Equal(t, len(resolution), 1)
	testutil.Equal(t, resolution[0].Status, "retried")
	requests := journal.byAction("grok_request")
	testutil.Equal(t, len(requests), 2)
	testutil.EqualAny(t, requests[0].Metadata["reasoning_effort"], "high")
	testutil.EqualAny(t, requests[1].Metadata["reasoning_effort"], "low")
	testutil.EqualAny(t, requests[1].Metadata["quality_check_status"], "passed")
}

// TestQualityGuardJournalsEveryStateChange follows one credential through the
// guard: parked, cleared by a healthy turn, parked again, then disabled.
func TestQualityGuardJournalsEveryStateChange(t *testing.T) {
	h, s, _ := setupValidationHandler(t)
	ctx := context.Background()
	acc := &store.Account{AccountType: "grok", Enabled: true, CredentialType: "oauth", GrokProvider: ProviderBuild, OAuthAccessToken: "jwt-user-guard"}
	testutil.NoError(t, s.CreateAccount(ctx, acc), "CreateAccount: %v")
	journal := &recordingAuditLogger{}
	h.SetAuditLogger(journal)
	req := &chatwire.Request{Model: "grok-4.5"}

	degraded := chatOutcome{Quality: qualitySignals{ExpectReasoning: true, Terminal: true, VisibleChars: 400}}
	healthy := chatOutcome{Quality: qualitySignals{ExpectReasoning: true, Terminal: true, VisibleChars: 400, SawReasoning: true, ReasoningChars: 200}}

	testutil.Equal(t, h.applyBuildQualityGuard(ctx, acc, req, degraded), qualityGuardParked)
	testutil.Equal(t, acc.QualityReason, qualityReasonNoReasoning)
	testutil.Equal(t, h.applyBuildQualityGuard(ctx, acc, req, healthy), qualityGuardCleared)
	testutil.Equal(t, acc.QualityReason, "")
	// A healthy turn with nothing to clear is not an event.
	testutil.Equal(t, h.applyBuildQualityGuard(ctx, acc, req, healthy), qualityGuardNone)
	testutil.Equal(t, h.applyBuildQualityGuard(ctx, acc, req, degraded), qualityGuardParked)
	testutil.Equal(t, h.applyBuildQualityGuard(ctx, acc, req, degraded), qualityGuardDisabled)

	rows := journal.byAction("grok_quality_guard")
	want := []qualityGuardAction{qualityGuardParked, qualityGuardCleared, qualityGuardParked, qualityGuardDisabled}
	testutil.Equal(t, len(rows), len(want))
	for index, row := range rows {
		testutil.Equal(t, row.Status, string(want[index]))
		testutil.Equal(t, row.Kind, audit.KindSystem)
		testutil.Equal(t, row.AccountID, acc.ID)
		testutil.Equal(t, row.Model, "grok-4.5")
	}
	testutil.EqualAny(t, rows[1].Metadata["previous_failures"], 1)
	testutil.EqualAny(t, rows[1].Metadata["failures"], 0)
	testutil.EqualAny(t, rows[3].Metadata["failures"], qualityDisableThreshold)
	testutil.EqualAny(t, rows[3].Metadata["account_enabled"], false)
	testutil.MustNotContain(t, journal.dump(t), "jwt-user-guard")
}

// TestClassifyQualityHoldReasonNamesTheDetector pins the reason to the detector
// that fired, and to the verdict: only a withhold carries a reason.
func TestClassifyQualityHoldReasonNamesTheDetector(t *testing.T) {
	cases := []struct {
		name   string
		sig    qualityStreamSignals
		reason string
	}{
		{"late short answer after a long reasoning bill",
			qualityStreamSignals{HoldExpired: true, VisibleTokens: 10, ReasoningTokens: 200, FirstVisible: true, VisibleFlushMS: 9000},
			qualityReasonBurstDump},
		{"cipher-only thinking with no bill while the answer ends",
			qualityStreamSignals{EncryptedBytes: 100, EncryptedFloor: 256, VisibleTokens: 50, Terminal: true, FirstVisible: true, VisibleFlushMS: 5000},
			qualityReasonCipherDrool},
		{"plaintext reasoning that is nearly the whole output, dumped at once",
			qualityStreamSignals{HasThinking: true, HasReasoningDelta: true, FirstVisible: true, Terminal: true, VisibleFlushMS: 100, ReasoningTokens: 90, OutputTokens: 100, VisibleTokens: 10},
			qualityReasonFastReasoningRatio},
		{"an answer with no thinking of any kind",
			qualityStreamSignals{VisibleTokens: 50, Terminal: true, FirstVisible: true, VisibleFlushMS: 5000},
			qualityReasonNoReasoning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict, reason := classifyQualityHoldReason(tc.sig, qualityHoldMinOutputDefault)
			testutil.Equal(t, verdict, qualityWithhold)
			testutil.Equal(t, reason, tc.reason)
			testutil.Equal(t, classifyQualityHold(tc.sig, qualityHoldMinOutputDefault), verdict)
		})
	}

	verdict, reason := classifyQualityHoldReason(qualityStreamSignals{HasThinking: true, HasReasoningDelta: true, VisibleTokens: 50, Terminal: true, FirstVisible: true, VisibleFlushMS: 5000, ReasoningTokens: 5, OutputTokens: 55}, qualityHoldMinOutputDefault)
	testutil.Equal(t, verdict, qualityDeliver)
	testutil.Equal(t, reason, "")
}
