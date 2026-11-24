package warp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"orchids-api/internal/config"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func testAccount() *store.Account {
	return &store.Account{AccountType: "warp", Enabled: true, WarpAccessToken: "synthetic-access", WarpRefreshToken: "synthetic-refresh", WarpExpiresAt: time.Now().Add(time.Hour), WarpModelIDs: []string{"model-a"}, WarpModelsSyncedAt: time.Now()}
}
func testRequest() upstream.UpstreamRequest {
	return upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "What is a test?"}}}}
}
func testConfig(base string) *config.Config {
	return &config.Config{WarpBaseURL: base, WarpFirebaseBaseURL: base, WarpTokenBaseURL: base, WarpFirebaseAPIKey: "synthetic-project-config"}
}

func TestUnsupportedRejectedBeforeNetwork(t *testing.T) {
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer s.Close()
	zero := 0
	falseValue := false
	temperature := 0.0
	mutations := []func(*upstream.UpstreamRequest){
		func(r *upstream.UpstreamRequest) { r.System = []prompt.SystemItem{{Text: "system"}} },
		func(r *upstream.UpstreamRequest) { r.Messages = append(r.Messages, prompt.Message{Role: "assistant"}) },
		func(r *upstream.UpstreamRequest) { r.Messages[0].Role = "system" },
		func(r *upstream.UpstreamRequest) { r.Tools = []any{map[string]any{"name": "x"}} },
		func(r *upstream.UpstreamRequest) { r.ToolChoice = "none" },
		func(r *upstream.UpstreamRequest) { r.ParallelToolCalls = &falseValue },
		func(r *upstream.UpstreamRequest) { r.MaxTokens = &zero },
		func(r *upstream.UpstreamRequest) { r.Temperature = &temperature },
		func(r *upstream.UpstreamRequest) { r.TopP = &temperature },
		func(r *upstream.UpstreamRequest) { r.Stop = []string{} },
		func(r *upstream.UpstreamRequest) { r.ResponseFormat = map[string]any{} },
		func(r *upstream.UpstreamRequest) { r.ResponseText = map[string]any{} },
		func(r *upstream.UpstreamRequest) { r.ReasoningEffort = "low" },
		func(r *upstream.UpstreamRequest) { r.ConversationID = "history" },
		func(r *upstream.UpstreamRequest) { r.Include = []string{} },
		func(r *upstream.UpstreamRequest) { r.PromptCacheKey = "cache" },
		func(r *upstream.UpstreamRequest) { r.Messages[0].Content.Text = strings.Repeat("好", 1366) },
		func(r *upstream.UpstreamRequest) {
			r.Messages[0].Content = prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "image_url", URL: "https://example.invalid"}}}
		},
		func(r *upstream.UpstreamRequest) { r.Model = "auto" },
	}
	for i, mutate := range mutations {
		r := testRequest()
		mutate(&r)
		c := NewFromAccount(&store.Account{WarpRefreshToken: "expired"}, testConfig(s.URL))
		err := c.SendRequestWithPayload(context.Background(), r, nil, nil)
		var app *apperrors.AppError
		if !errors.As(err, &app) || app.HTTPStatus != 400 {
			t.Fatalf("case %d not rejected as 400: %v", i, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unsupported input reached network")
	}
}
func TestGenerationExactRequestAndSafeEmission(t *testing.T) {
	stream := testSSE(t, testMessages(testOutput("m", "safe answer"))) + testSSE(t, testDone())
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/ai/multi-agent" || r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Error("wrong generation request")
		}
		raw, _ := io.ReadAll(r.Body)
		var wire warpapi.Request
		if proto.Unmarshal(raw, &wire) != nil || wire.GetInput().GetUserInputs().GetInputs()[0].GetUserQuery().GetQuery() != "What is a test?" {
			t.Error("wrong protobuf input")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	defer s.Close()
	var events []upstream.SSEMessage
	err := NewFromAccount(testAccount(), testConfig(s.URL)).SendRequestWithPayload(context.Background(), testRequest(), func(m upstream.SSEMessage) { events = append(events, m) }, nil)
	if err != nil || calls.Load() != 1 || len(events) != 2 || events[0].Type != "model.text-delta" || events[0].Event["delta"] != "safe answer" || events[1].Type != "model.finish" {
		t.Fatalf("generation failed: %v", err)
	}
}
func TestToolFailureDoesNotLeakPartialOrReplay(t *testing.T) {
	stream := testSSE(t, testMessages(testOutput("m", "must not leak"))) + testSSE(t, testMessages(warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{ApplyFileDiffs: warpapi.Message_ToolCall_ApplyFileDiffs_builder{}.Build()}.Build()}.Build())) + testSSE(t, testDone())
	var calls atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	defer s.Close()
	emitted := false
	err := NewFromAccount(testAccount(), testConfig(s.URL)).SendRequestWithPayload(context.Background(), testRequest(), func(upstream.SSEMessage) { emitted = true }, nil)
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != 502 || calls.Load() != 1 || emitted {
		t.Fatal("unsafe output or generation replay")
	}
}
func TestCatalogOnlyEnabledExactFreshModels(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql/v2" {
			t.Error("wrong catalog path")
		}
		_, _ = w.Write([]byte(`{"data":{"user":{"__typename":"UserOutput","user":{"workspaces":[{"featureModelChoice":{"agentMode":{"choices":[{"id":"model-a","disableReason":null},{"id":"model-b","disableReason":"upgrade"},{"id":"model-a"},{"id":"auto"},{"id":"all"},{"id":"warp-auto"}]}}}]}}}}`))
	}))
	defer s.Close()
	choices, err := NewFromAccount(testAccount(), testConfig(s.URL)).FetchModelChoices(t.Context())
	if err != nil || len(choices) != 1 || choices[0].ID != "model-a" {
		t.Fatal("catalog not filtered")
	}
	acc := testAccount()
	if !(capabilities{}).SupportsModel(acc, "model-a") || (capabilities{}).SupportsModel(acc, "MODEL-A") || (capabilities{}).SupportsModel(nil, "model-a") {
		t.Fatal("non-exact snapshot")
	}
	acc.WarpModelsSyncedAt = time.Now().Add(-provider.HealthRefreshInterval - time.Second)
	if (capabilities{}).SupportsModel(acc, "model-a") || len(catalogPort().LastKnown(acc)) != 0 {
		t.Fatal("stale fallback allowed")
	}
}
func TestGenerationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := NewFromAccount(testAccount(), testConfig("http://127.0.0.1:1")).SendRequestWithPayload(ctx, testRequest(), nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
func TestRedactedErrorsAndRedirect(t *testing.T) {
	forwarded := false
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded = true }))
	defer dest.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()
	c := NewFromAccount(testAccount(), testConfig(src.URL))
	var answer any
	err := c.json(context.Background(), src.URL+"?key=synthetic-private", "application/json", nil, "synthetic-bearer", &answer)
	if err == nil || forwarded || strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), src.URL) {
		t.Fatal("unsafe redirect or error")
	}
}
