package grok

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestRestrictionsToolNamesRemainCaseSensitiveAndRoundTrip(t *testing.T) {
	tools := []ToolDef{{Type: "function", Function: map[string]interface{}{"name": "ReadFile"}}, {Type: "function", Function: map[string]interface{}{"name": "readfile"}}}
	req := ChatCompletionsRequest{Model: "grok-4.6", Messages: []ChatMessage{{Role: "user", Content: "hi"}}, Tools: tools,
		ToolChoice: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "ReadFile"}}}
	if err := req.Validate(); err != nil {
		t.Fatal(err)
	}
	req.ToolChoice.(map[string]interface{})["function"].(map[string]interface{})["name"] = "READFILE"
	if err := req.Validate(); err == nil {
		t.Fatal("forced names must match exactly")
	}
	if err := validateToolDefinitions(append(tools, tools[0])); err == nil {
		t.Fatal("exact duplicates must fail")
	}
	longName := strings.Repeat("Long", 40)
	declarations := []map[string]interface{}{{"type": "function", "name": "a b"}, {"type": "function", "name": "a_b_2"}, {"type": "function", "name": "a@b"}, {"type": "function", "name": longName}, {"type": "function", "name": "ReadFile"}, {"type": "function", "name": "readfile"}}
	payload := map[string]interface{}{"tools": declarations, "tool_choice": map[string]interface{}{"type": "function", "name": longName}, "input": []interface{}{map[string]interface{}{"type": "function_call", "name": longName, "call_id": "call_x", "arguments": "{}"}}}
	aliases := collectBuildToolAliases(payload)
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, tool := range interfaceMaps(payload["tools"]) {
		name := tool["name"].(string)
		if seen[name] || len(name) > 128 {
			t.Fatalf("invalid alias %q", name)
		}
		seen[name] = true
	}
	alias := payload["tool_choice"].(map[string]interface{})["name"].(string)
	if interfaceMaps(payload["input"])[0]["name"] != alias {
		t.Fatal("history and declaration aliases differ")
	}
	raw, _ := json.Marshal(map[string]interface{}{"type": "function_call", "name": alias, "call_id": "call_x", "arguments": "{}"})
	var restored map[string]interface{}
	_ = json.Unmarshal(rewriteBuildToolAliasesJSON(raw, aliases), &restored)
	if restored["name"] != longName {
		t.Fatalf("round trip lost original name: %v", restored)
	}
}

// validatePayloadReasoning only checks structure and never rewrites the caller's
// value. The wire normalization that follows maps client aliases onto the levels
// each model actually accepts.
func TestRestrictionsReasoningAliasesReachWire(t *testing.T) {
	for _, test := range []struct{ model, effort, wire string }{
		{"grok-4.6", "max", "xhigh"},
		{"grok-4.5", "max", "high"},
		{"grok-4.5", "xhigh", "high"},
		{"grok-3-mini", "medium", "medium"},
		{"grok-3-mini-fast", "minimal", "low"},
	} {
		spec := ModelSpec{ID: test.model, UpstreamModel: test.model, Upstream: UpstreamCLI}
		payload := map[string]interface{}{"reasoning": map[string]interface{}{"effort": test.effort, "summary": "auto"}}
		if err := validatePayloadReasoning(payload); err != nil {
			t.Fatal(test, err)
		}
		if payload["reasoning"].(map[string]interface{})["effort"] != test.effort {
			t.Fatal(test, payload)
		}
		effort := test.effort
		request := &ChatCompletionsRequest{Model: test.model, Messages: []ChatMessage{{Role: "user", Content: "hi"}}, ReasoningEffort: &effort}
		if err := request.Validate(); err != nil {
			t.Fatal(test, err)
		}
		chat, err := (&Handler{}).responsesPayloadFromChat(spec, request, true)
		if err != nil || chat["reasoning"].(map[string]interface{})["effort"] != test.wire {
			t.Fatal(test, chat, err)
		}
	}
}

func TestRestrictionsEmptyAndImageToolOutputs(t *testing.T) {
	for _, content := range []interface{}{"", []interface{}{map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/a.png", "detail": "high"}}}} {
		messages := []ChatMessage{{Role: "tool", ToolCallID: "call_a", Content: content}}
		if err := validateChatMessages(messages); err != nil {
			t.Fatal(err)
		}
		input, _ := responsesInputFromChatMessages(messages)
		item := input[0].(map[string]interface{})
		if item["call_id"] != "call_a" {
			t.Fatal(item)
		}
		if content == "" && item["output"] != "" {
			t.Fatal("empty result changed")
		}
		if parts, ok := item["output"].([]interface{}); ok {
			if parts[0].(map[string]interface{})["detail"] != "high" {
				t.Fatal(parts)
			}
		}
	}
	if err := validateChatMessages([]ChatMessage{{Role: "tool", Content: ""}}); err == nil {
		t.Fatal("missing call ID accepted")
	}
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
		if (err != nil) != test.blocked {
			t.Fatalf("%s %s blocked=%v: %v", test.provider, test.model, test.blocked, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for i := 0; i < 100; i++ {
		if err := waitScopedRateLimit(ctx, ProviderBuild, "unlimited-default", "m", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitScopedRateLimit(ctx, ProviderBuild, "paced-account-a", "m", 0.1); err != nil {
		t.Fatal(err)
	}
	short, stop := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stop()
	if err := waitScopedRateLimit(short, ProviderBuild, "paced-account-a", "m", 0.1); err == nil {
		t.Fatal("configured pace ignored")
	}
	if err := waitScopedRateLimit(ctx, ProviderBuild, "paced-account-b", "m", 0.1); err != nil {
		t.Fatal("unrelated account blocked", err)
	}
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
	h, s, mini := setupValidationHandler(t)
	defer s.Close()
	defer mini.Close()
	model := "grok-4.6"
	if err := s.CreateModel(context.Background(), &store.Model{Channel: "Grok", ModelID: model, Name: model, Status: store.ModelStatusAvailable, Verified: true}); err != nil {
		t.Fatal(err)
	}
	acc := &store.Account{AccountType: "grok", GrokProvider: ProviderBuild, CredentialType: "oauth", Enabled: true, OAuthAccessToken: jwtWithClaims(t, `{"sub":"restriction-user","team_id":"restriction-build"}`), OAuthExpiresAt: time.Now().Add(time.Hour), GrokModels: []string{model}, GrokModelsSyncedAt: time.Now()}
	if err := s.CreateAccount(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	h.cfg = &config.Config{GrokCLIBaseURL: upstream.URL + "/v1"}
	h.cliClient = NewCLIClient(h.cfg)
	h.cliClient.httpClient = upstream.Client()
	h.cliClient.oauth.httpClient = upstream.Client()
	longName := strings.Repeat("Tool", 50)
	tools := make([]ToolDef, 129)
	for i := range tools {
		tools[i] = ToolDef{Type: "function", Function: map[string]interface{}{"name": fmt.Sprintf("tool_%d", i)}}
	}
	tools[0].Function["name"] = longName
	body, _ := json.Marshal(ChatCompletionsRequest{Model: model, Messages: []ChatMessage{{Role: "user", Content: "use the tool"}}, Tools: tools, ToolChoice: map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": longName}}})
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), longName) || !strings.Contains(rec.Body.String(), "call_long") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	select {
	case payload := <-received:
		if len(interfaceMaps(payload["tools"])) != 129 {
			t.Fatal("tool list truncated")
		}
		if name := parseLooseStringAny(payload["tool_choice"].(map[string]interface{})["name"]); name == "" || len(name) > 128 {
			t.Fatal("invalid wire alias", name)
		}
	default:
		t.Fatal("Build was not called")
	}
}
