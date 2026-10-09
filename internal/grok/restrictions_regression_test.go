package grok

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// validatePayloadReasoning only checks structure and never rewrites the caller's
// value. The wire normalization that follows maps client aliases onto the levels
// each model actually accepts; every one of those mappings (max/xhigh on 4.6 and
// 4.5, and minimal) is pinned by relay_policy_test.go
// (TestRelayChatSamplingAndEffortAreClientOwned and
// TestRelayBuildEffortAliasesFollowModelContract), so this test guards the
// no-rewrite half only.
func TestRestrictionsReasoningAliasesReachWire(t *testing.T) {
	for _, test := range []struct{ model, effort string }{
		{"grok-4.6", "max"},
		{"grok-4.5", "max"},
		{"grok-4.5", "xhigh"},
		{"grok-3-mini", "medium"},
		{"grok-3-mini-fast", "minimal"},
	} {
		payload := map[string]interface{}{"reasoning": map[string]interface{}{"effort": test.effort, "summary": "auto"}}
		err := validatePayloadReasoning(payload)
		testutil.NoError(t, err)
		testutil.Fail(t, payload["reasoning"].(map[string]interface{})["effort"] != test.effort, test, payload)
		effort := test.effort
		request := &chatwire.Request{Model: test.model, Messages: []chatwire.Message{{Role: "user", Content: "hi"}}, ReasoningEffort: &effort}
		err = request.Validate()
		testutil.NoError(t, err)
	}
}

func TestRestrictionsEmptyAndImageToolOutputs(t *testing.T) {
	for _, content := range []interface{}{"", []interface{}{map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/a.png", "detail": "high"}}}} {
		messages := []chatwire.Message{{Role: "tool", ToolCallID: "call_a", Content: content}}
		testutil.NoError(t, chatwire.ValidateMessages(messages))
		input, _ := responsesInputFromChatMessages(messages)
		item := input[0].(map[string]interface{})
		testutil.Equal(t, item["call_id"], "call_a")
		testutil.False(t, content == "" && item["output"] != "", "empty result changed")
		if parts, ok := item["output"].([]interface{}); ok {
			testutil.Fail(t, parts[0].(map[string]interface{})["detail"] != "high", parts)
		}
	}
	err := chatwire.ValidateMessages([]chatwire.Message{{Role: "tool", Content: ""}})
	testutil.Error(t, err)
}

func TestRestrictionsScopedCooldownAndPacing(t *testing.T) {
	old := teamCooldown
	teamCooldown = newTeamCooldownRegistry()
	defer func() { teamCooldown = old }()
	endpointRateLimiterMu.Lock()
	oldPacing := endpointRateLimiters
	endpointRateLimiters = map[string]*tokenBucket{}
	endpointRateLimiterMu.Unlock()
	defer func() {
		endpointRateLimiterMu.Lock()
		endpointRateLimiters = oldPacing
		endpointRateLimiterMu.Unlock()
	}()
	ctxA := withRateLimitAccount(context.Background(), &store.Account{ID: 1, TeamID: "restriction-team-a"})
	ctxSibling := withRateLimitAccount(context.Background(), &store.Account{ID: 2, TeamID: "restriction-team-a"})
	ctxB := withRateLimitAccount(context.Background(), &store.Account{ID: 3, TeamID: "restriction-team-b"})
	noteScopedRateLimit(ctxA, ProviderBuild, "tokenA", "grok-4.3", 429, http.Header{"Retry-After": []string{"2"}}, []byte(`{"error":"limited"}`))
	if remaining := teamCooldown.RetryAfterFor(RateLimitScopeRPM, ProviderBuild+":team:restriction-team-a", "grok-4.3"); remaining <= time.Second || remaining > 2*time.Second {
		t.Fatal(remaining)
	}
	for _, test := range []struct {
		ctx             context.Context
		provider, model string
		blocked         bool
	}{{ctxSibling, ProviderBuild, "grok-4.3", true}, {ctxB, ProviderBuild, "grok-4.3", false}, {ctxA, ProviderBuild, "grok-4.7", false}, {ctxA, ProviderBuild, "grok-4.6", false}} {
		ctx, cancel := context.WithTimeout(test.ctx, 20*time.Millisecond)
		err := waitScopedRateLimit(ctx, test.provider, "unused", test.model, 0)
		cancel()
		testutil.Falsef(t, (err != nil) != test.blocked, "%s %s blocked=%v: %v", test.provider, test.model, test.blocked, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		testutil.NoError(t, waitScopedRateLimit(ctx, ProviderBuild, "unlimited-default", "m", 0))
	}
	testutil.NoError(t, waitScopedRateLimit(ctx, ProviderBuild, "paced-account-a", "m", 0.1))
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	err := waitScopedRateLimit(short, ProviderBuild, "paced-account-a", "m", 0.1)
	testutil.Error(t, err)
	testutil.NoError(t, waitScopedRateLimit(ctx, ProviderBuild, "paced-account-b", "m", 0.1), "unrelated account blocked")
}

func TestRestrictionsBuildChatLongToolNameEndToEnd(t *testing.T) {
	received := make(chan map[string]interface{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		received <- payload
		choice, _ := payload["tool_choice"].(map[string]interface{})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "resp_tool", "status": "completed", "output": []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_long", "name": choice["name"], "arguments": "{}"}}})
	}))
	defer upstream.Close()
	h, s, _ := setupValidationHandler(t)
	model := "grok-4.6"
	testutil.NoError(t, s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: model, Name: model, Status: store.ModelStatusAvailable, Verified: true}))
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true, OAuthAccessToken: jwtWithClaims(t, `{"sub":"restriction-user","team_id":"restriction-build"}`), OAuthExpiresAt: time.Now().Add(time.Hour), GrokModels: []string{model}, GrokModelsSyncedAt: time.Now()}
	testutil.NoError(t, s.CreateAccount(context.Background(), acc))
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
	longName := strings.Repeat("Tool", 50)
	tools := make([]chatwire.ToolDef, 129)
	for i := range tools {
		tools[i] = chatwire.ToolDef{Type: "function", Function: map[string]interface{}{"name": fmt.Sprintf("tool_%d", i)}}
	}
	tools[0].Function["name"] = longName
	body, _ := json.Marshal(chatwire.Request{Model: model, Messages: []chatwire.Message{{Role: "user", Content: "use the tool"}}, Tools: tools, ToolChoice: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": longName}}})
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	testutil.Falsef(t, rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), longName) || !strings.Contains(rec.Body.String(), "call_long"), "%d %s", rec.Code, rec.Body.String())
	select {
	case payload := <-received:
		testutil.Equal(t, len(responses.InterfaceMaps(payload["tools"])), 129)
		if name := chatwire.ParseLooseStringAny(payload["tool_choice"].(map[string]interface{})["name"]); name != longName {
			t.Fatal("tool name changed", name)
		}
	default:
		t.Fatal("Build was not called")
	}
}
