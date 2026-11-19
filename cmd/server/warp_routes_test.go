package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

const (
	warpRouteModel       = "claude-e2e-exact"
	warpRouteQuery       = "Explain this isolated test. 好"
	warpRouteAnswer      = "Warp route answer."
	warpRouteMCPServerID = "d8db81de-9599-531a-9f9f-9053f843a477"
)

type warpRouteStub struct {
	*httptest.Server
	aiCalls    atomic.Int64
	otherCalls atomic.Int64
	requests   chan []byte
}

// The upstream speaks Warp's official protobuf SSE, not the gateway's internal
// event format. Every configured upstream origin points to this listener.
func newWarpRouteStub(t *testing.T, status int, precedingMessages ...*warpapi.Message) *warpRouteStub {
	t.Helper()
	id, text := "answer-e2e", warpRouteAnswer
	answer := warpapi.ResponseEvent_builder{ClientActions: warpapi.ResponseEvent_ClientActions_builder{
		Actions: []*warpapi.ClientAction{warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
			Messages: append(precedingMessages, warpapi.Message_builder{Id: &id, AgentOutput: warpapi.Message_AgentOutput_builder{Text: &text}.Build()}.Build()),
		}.Build()}.Build()},
	}.Build()}.Build()
	done := warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{
		Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build(),
	}.Build()}.Build()
	var stream strings.Builder
	for _, event := range []*warpapi.ResponseEvent{answer, done} {
		raw, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&stream, "data: %s\n\n", base64.RawURLEncoding.EncodeToString(raw))
	}
	stub := &warpRouteStub{requests: make(chan []byte, 16)}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ai/multi-agent" {
			stub.otherCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		stub.aiCalls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-protobuf" || r.Header.Get("Accept") != "text/event-stream" {
			t.Error("Warp generation must POST protobuf and accept SSE")
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-warp-access" {
			t.Error("Warp generation did not use its planted access token")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("read Warp generation body:", err)
		}
		stub.requests <- raw
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":"synthetic upstream failure"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream.String())
	}))
	t.Cleanup(stub.Close)
	return stub
}

func newWarpRouteE2E(t *testing.T, stub *warpRouteStub, accounts int) (*channelE2E, *store.Store, *handler.Handler) {
	t.Helper()
	cfg := &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin",
		WarpBaseURL: stub.URL, WarpFirebaseBaseURL: stub.URL, WarpTokenBaseURL: stub.URL,
		WarpFirebaseAPIKey: "synthetic-project-config", MaxRetries: 3, RetryDelay: 1}
	mux, s, h := newRouteMux(t, "warp-routes:", cfg)
	for i := 0; i < accounts; i++ {
		acc := &store.Account{Name: fmt.Sprintf("warp-e2e-%d", i), AccountType: "warp", Enabled: true, Weight: 1,
			WarpAccessToken: "synthetic-warp-access", WarpRefreshToken: "synthetic-warp-refresh",
			WarpExpiresAt: time.Now().Add(time.Hour), WarpModelIDs: []string{warpRouteModel}, WarpModelsSyncedAt: time.Now()}
		if err := s.CreateAccount(context.Background(), acc); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.ReconcileDiscoveredModels(context.Background(), "warp", []*store.Model{{Channel: "warp", ModelID: warpRouteModel,
		Name: warpRouteModel, UpstreamModel: warpRouteModel, Provider: "warp", Status: store.ModelStatusAvailable,
		Verified: true, Capabilities: []string{store.CapabilityChat, "text"}}}, store.ModelReconcileOptions{Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	return newChannelE2EWith(t, mux, s, h, "sk-warp-routes"), s, h
}

// Observe the shared payload without replacing the real registered client.
// These fields are not represented by Warp's protobuf, so checking only the
// upstream wire could miss a shared-pipeline default that the client discarded.
type warpRoutePayloadObserver struct {
	next     handler.UpstreamClient
	payloads chan upstream.UpstreamRequest
}

func (c *warpRoutePayloadObserver) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, emit func(upstream.SSEMessage), logger *debug.Logger) error {
	c.payloads <- req
	return c.next.SendRequestWithPayload(ctx, req, emit, logger)
}

func assertWarpRouteRequest(t *testing.T, raw []byte) {
	t.Helper()
	var request warpapi.Request
	if err := proto.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	settings := request.GetSettings()
	model := settings.GetModelConfig()
	if model.GetBase() != warpRouteModel || model.HasCliAgent() || model.HasComputerUseAgent() || model.HasPlanning() || model.HasCoding() {
		t.Fatal("Warp must send the exact model without CLI or subagent model configuration")
	}
	inputs := request.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != warpRouteQuery {
		t.Fatal("Warp did not send exactly the original user text")
	}
	if request.GetMcpContext() != nil || len(request.GetTaskContext().GetTasks()) != 0 || request.GetInput().GetContext().HasCurrentTime() || request.GetMetadata().GetConversationId() != "" {
		t.Fatal("Warp request acquired tools, history, current time or conversation context")
	}
	fields := settings.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		if field.Kind() == protoreflect.BoolKind {
			want := string(field.Name()) == "should_preserve_file_content_in_history"
			if !settings.ProtoReflect().Has(field) || settings.ProtoReflect().Get(field).Bool() != want {
				t.Errorf("Warp capability %s has incorrect value", field.Name())
			}
		}
	}
	for _, tools := range [][]warpapi.ToolType{settings.GetSupportedTools(), settings.GetSupportedCliAgentTools()} {
		if len(tools) != 1 || tools[0] != warpapi.ToolType_SUGGEST_PROMPT {
			t.Fatal("Warp must retain its non-executable passive tool fence")
		}
	}
	if settings.GetAutonomyLevel() != warpapi.AutonomyLevel_SUPERVISED || settings.GetIsolationLevel() != warpapi.IsolationLevel_SANDBOX {
		t.Fatal("Warp must not enable autonomous execution")
	}
}

func TestWarpChatCompletionsSharedPipeline(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, s, h := newWarpRouteE2E(t, stub, 1)
	payloads := make(chan upstream.UpstreamRequest, 8)
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
		spec, ok := provider.Lookup(acc.AccountType)
		if !ok || spec.NewClient == nil {
			t.Fatal("Warp provider is not registered")
		}
		return &warpRoutePayloadObserver{next: spec.NewClient(acc, cfg, s), payloads: payloads}
	})
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", prefix, streaming), func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"model": warpRouteModel, "stream": streaming,
					"messages": []map[string]string{{"role": "user", "content": warpRouteQuery}}})
				if err != nil {
					t.Fatal(err)
				}
				resp := e.do(t, http.MethodPost, prefix+"/chat/completions", string(body), false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("chat = %d %s", resp.StatusCode, out)
				}
				if streaming {
					if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, `"object":"chat.completion.chunk"`) || !strings.Contains(out, `"content":"`+warpRouteAnswer+`"`) || !strings.Contains(out, `"finish_reason":"stop"`) || strings.Count(out, "data: [DONE]") != 1 {
						t.Fatalf("invalid client SSE: %s", out)
					}
				} else {
					var completion struct {
						Object  string `json:"object"`
						Model   string `json:"model"`
						Choices []struct {
							Message      struct{ Role, Content string }
							FinishReason string `json:"finish_reason"`
						} `json:"choices"`
					}
					if err := json.Unmarshal([]byte(out), &completion); err != nil {
						t.Fatal(err)
					}
					if completion.Object != "chat.completion" || completion.Model != warpRouteModel || len(completion.Choices) != 1 || completion.Choices[0].Message.Role != "assistant" || completion.Choices[0].Message.Content != warpRouteAnswer || completion.Choices[0].FinishReason != "stop" {
						t.Fatalf("invalid client JSON: %s", out)
					}
				}
				select {
				case payload := <-payloads:
					if len(payload.System) != 0 || payload.MaxTokens != nil || payload.Model != warpRouteModel || len(payload.Messages) != 1 || payload.Messages[0].Content.Text != warpRouteQuery {
						t.Fatal("shared pipeline injected system/default MaxTokens or changed the user message/model")
					}
				default:
					t.Fatal("request bypassed the shared provider pipeline")
				}
				select {
				case raw := <-stub.requests:
					assertWarpRouteRequest(t, raw)
				default:
					t.Fatal("no upstream generation")
				}
			})
		}
	}
	if stub.aiCalls.Load() != 4 || stub.otherCalls.Load() != 0 {
		t.Fatalf("upstream calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}

// Model identity stays exact: auto-routing aliases and requests without a model
// cannot be rewritten to a catalog model, so they fail before any paid call.
// Client tools and advisory generation controls are accepted and covered by the
// dedicated tests below.
func TestWarpRejectsModelSubstitutionBeforeUpstream(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	cases := map[string]string{
		"auto":          `{"model":"auto","messages":[{"role":"user","content":"hello"}]}`,
		"omitted_model": `{"messages":[{"role":"user","content":"hello"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", body, false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("invalid request = %d %s", resp.StatusCode, out)
			}
			if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
				t.Fatal("unsupported request reached upstream")
			}
		})
	}
}

// Anthropic and OpenAI clients always send generation controls the Warp agent
// endpoint cannot express. They are accepted for compatibility but not
// forwarded; the gateway's own byte ceilings still bound the answer.
func TestWarpAcceptsAdvisoryGenerationControlsWithoutForwarding(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	body := `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"hello"}],"max_tokens":32,"temperature":0,"top_p":0.9,"stop":["x"]}`
	resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", body, false)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK || stub.aiCalls.Load() != 1 {
		t.Fatalf("advisory controls = %d %s (calls=%d)", resp.StatusCode, out, stub.aiCalls.Load())
	}
}

// A short instruction message is folded into the single Warp query; it must not
// be treated as history or as a separate paid turn.
func TestWarpAcceptsOneBoundedSystemMessage(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	body := `{"model":"` + warpRouteModel + `","messages":[{"role":"system","content":"Answer briefly."},{"role":"user","content":"hello"}],"stream":false}`
	resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", body, false)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK || stub.aiCalls.Load() != 1 {
		t.Fatalf("bounded system request = %d %s (calls=%d)", resp.StatusCode, out, stub.aiCalls.Load())
	}
}

// Warp serves the Messages and Responses wire formats through the shared
// bridges in routes.go, so those paths must authenticate before routing: an
// unauthenticated request is a 401, never the "404 page not found" of an
// unregistered route.
func TestWarpProtocolRoutesExistAndAuthenticate(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	unauthorized := func(method, path string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(method, e.server.URL+path, strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp, e.readBody(t, resp)
	}
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, route := range []struct{ method, suffix string }{
			{http.MethodPost, "/messages"},
			{http.MethodPost, "/messages/count_tokens"},
			{http.MethodPost, "/responses"},
			{http.MethodPost, "/responses/compact"},
			{http.MethodGet, "/responses/resp_test"},
			{http.MethodPost, "/responses/resp_test/cancel"},
			{http.MethodGet, "/responses/resp_test/input_items"},
		} {
			resp, out := unauthorized(route.method, prefix+route.suffix)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s%s without key = %d %s", prefix, route.suffix, resp.StatusCode, out)
			}
			if strings.Contains(out, "404 page not found") {
				t.Fatalf("%s%s is still unrouted: %s", prefix, route.suffix, out)
			}
		}
		// With a managed key the POST-only endpoints answer their method guard,
		// which is the difference between "this endpoint rejects GET" and "this
		// endpoint does not exist".
		for _, suffix := range []string{"/messages", "/messages/count_tokens", "/responses", "/responses/compact"} {
			resp := e.do(t, http.MethodGet, prefix+suffix, "", false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusMethodNotAllowed {
				t.Fatalf("%s%s method guard = %d %s", prefix, suffix, resp.StatusCode, out)
			}
		}
		// Resource routes resolve from the shared store without an upstream call.
		for _, route := range []struct{ method, suffix string }{
			{http.MethodGet, "/responses/resp_absent"},
			{http.MethodPost, "/responses/resp_absent/cancel"},
			{http.MethodGet, "/responses/resp_absent/input_items"},
		} {
			resp := e.do(t, route.method, prefix+route.suffix, "", false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusNotFound || !strings.Contains(out, "response_not_found") {
				t.Fatalf("%s%s missing resource = %d %s", prefix, route.suffix, resp.StatusCode, out)
			}
		}
		for _, auth := range []string{"", "Bearer admintoken"} {
			req, err := http.NewRequest(http.MethodGet, e.server.URL+prefix+"/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", auth)
			req.Header.Set("X-Admin-Token", "admintoken")
			resp, err := e.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("models without managed key = %d %s", resp.StatusCode, out)
			}
		}
		resp := e.do(t, http.MethodGet, prefix+"/models", "", false)
		out := e.readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("models = %d %s", resp.StatusCode, out)
		}
		var catalog struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &catalog); err != nil {
			t.Fatal(err)
		}
		if len(catalog.Data) != 1 || catalog.Data[0].ID != warpRouteModel {
			t.Fatalf("unexpected catalog or default alias: %s", out)
		}
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/api/warp/login"},
		{http.MethodGet, "/api/warp/login/session-e2e"},
		{http.MethodDelete, "/api/warp/login/session-e2e"},
	} {
		for _, authorization := range []string{"", "Bearer " + e.managedKey} {
			req, err := http.NewRequest(route.method, e.server.URL+route.path, strings.NewReader(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", authorization)
			resp, err := e.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("management %s %s without admin session = %d %s", route.method, route.path, resp.StatusCode, out)
			}
		}
	}
	// Admin authentication reaches the transaction handler without starting a
	// device authorization or making any upstream request.
	resp := e.do(t, http.MethodGet, "/api/warp/login/session-e2e", "", true)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(out, "login_not_found") {
		t.Fatalf("authenticated management lookup = %d %s", resp.StatusCode, out)
	}
	if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
		t.Fatal("route/auth checks unexpectedly reached upstream")
	}
}

// The Messages and Responses bridges share the chat handler, so a tool-free
// single-turn request must reach Warp's real client and be answered end-to-end
// on both provider bases.
func TestWarpBridgedMessagesAndResponsesToolFree(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		t.Run(prefix+"/messages", func(t *testing.T) {
			body := `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"` + warpRouteQuery + `"}],"stream":false}`
			resp := e.do(t, http.MethodPost, prefix+"/messages", body, false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("messages = %d %s", resp.StatusCode, out)
			}
			var message struct {
				Type       string `json:"type"`
				Role       string `json:"role"`
				Model      string `json:"model"`
				StopReason string `json:"stop_reason"`
				Content    []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal([]byte(out), &message); err != nil {
				t.Fatal(err)
			}
			if message.Type != "message" || message.Role != "assistant" || message.Model != warpRouteModel ||
				message.StopReason != "end_turn" || len(message.Content) != 1 ||
				message.Content[0].Type != "text" || message.Content[0].Text != warpRouteAnswer {
				t.Fatalf("invalid Anthropic bridge response: %s", out)
			}
			select {
			case raw := <-stub.requests:
				assertWarpRouteRequest(t, raw)
			default:
				t.Fatal("Messages request bypassed the shared Warp pipeline")
			}
		})
		t.Run(prefix+"/responses", func(t *testing.T) {
			body := `{"model":"` + warpRouteModel + `","input":"` + warpRouteQuery + `"}`
			resp := e.do(t, http.MethodPost, prefix+"/responses", body, false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("responses = %d %s", resp.StatusCode, out)
			}
			var response struct {
				Object string `json:"object"`
				Status string `json:"status"`
				Model  string `json:"model"`
				Output []struct {
					Type    string `json:"type"`
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			}
			if err := json.Unmarshal([]byte(out), &response); err != nil {
				t.Fatal(err)
			}
			answer := ""
			for _, item := range response.Output {
				if item.Type != "message" || item.Role != "assistant" {
					continue
				}
				for _, part := range item.Content {
					if part.Type == "output_text" {
						answer = part.Text
					}
				}
			}
			if response.Object != "response" || response.Status != "completed" || response.Model != warpRouteModel || answer != warpRouteAnswer {
				t.Fatalf("invalid Responses bridge response: %s", out)
			}
			select {
			case raw := <-stub.requests:
				assertWarpRouteRequest(t, raw)
			default:
				t.Fatal("Responses request bypassed the shared Warp pipeline")
			}
		})
	}
	if stub.aiCalls.Load() != 4 || stub.otherCalls.Load() != 0 {
		t.Fatalf("bridged tool-free calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}

// Warp's client tools are declared to upstream as MCP tools, so a bridged
// request that carries OpenAI function declarations must keep them rather than
// silently dropping them before the paid turn.
func TestWarpBridgedOpenAIToolsReachUpstreamDeclared(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, tc := range []struct{ name, path, body string }{
			{
				name: "messages",
				path: "/messages",
				body: `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"` + warpRouteQuery + `"}],
					"tools":[{"type":"function","function":{"name":"run","description":"run a command","parameters":{"type":"object"}}}]}`,
			},
			{
				name: "responses",
				path: "/responses",
				body: `{"model":"` + warpRouteModel + `","input":"` + warpRouteQuery + `",
					"tools":[{"type":"function","name":"run","parameters":{"type":"object"}}]}`,
			},
		} {
			t.Run(prefix+"/"+tc.name, func(t *testing.T) {
				resp := e.do(t, http.MethodPost, prefix+tc.path, tc.body, false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s with tools = %d %s", tc.name, resp.StatusCode, out)
				}
				select {
				case raw := <-stub.requests:
					var request warpapi.Request
					if err := proto.Unmarshal(raw, &request); err != nil {
						t.Fatal(err)
					}
					// Warp may declare client tools either directly on the MCP
					// context or on a client server inside it; both prove the
					// declaration survived the shared bridge.
					mcp := request.GetMcpContext()
					declared := mcp.GetTools()
					for _, server := range mcp.GetServers() {
						declared = append(declared, server.GetTools()...)
					}
					if len(declared) != 1 || declared[0].GetName() != "run" || declared[0].GetInputSchema() == nil {
						t.Fatalf("%s lost its client tool: %+v", tc.name, declared)
					}
					inputs := request.GetInput().GetUserInputs().GetInputs()
					if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != warpRouteQuery {
						t.Fatal("tool request did not keep the single user query")
					}
				default:
					t.Fatalf("%s with tools did not reach upstream", tc.name)
				}
			})
		}
	}
	if stub.aiCalls.Load() != 4 || stub.otherCalls.Load() != 0 {
		t.Fatalf("tool-bearing calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}

// Anthropic-shaped tools are what Claude Code and the Anthropic SDK send; the
// raw gate must accept the shape so the shared translator can normalise it.
func TestWarpMessagesAcceptsAnthropicToolShape(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	body := `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"run","description":"run a command","input_schema":{"type":"object"}}]}`
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		if prefix == "/warp/v1" {
			// One paid call is enough to prove the shape; the second prefix is a
			// route alias, so keep the assertion scoped to the first call.
			break
		}
		t.Run(prefix, func(t *testing.T) {
			resp := e.do(t, http.MethodPost, prefix+"/messages", body, false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Anthropic-shaped tools = %d %s", resp.StatusCode, out)
			}
			if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
				t.Fatalf("Anthropic-shaped tool call: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
			}
		})
	}
}

func TestWarpSharedPipelineSingleGenerationAttempt(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusInternalServerError)
	e, _, _ := newWarpRouteE2E(t, stub, 2)
	spec, ok := provider.Lookup("warp")
	if !ok || !spec.SingleGenerationAttempt {
		t.Fatal("Warp must register SingleGenerationAttempt")
	}
	resp := e.do(t, http.MethodPost, "/warp/v1/chat/completions", `{"model":"`+warpRouteModel+`","messages":[{"role":"user","content":"`+warpRouteQuery+`"}]}`, false)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream failure = %d %s", resp.StatusCode, out)
	}
	if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
		t.Fatalf("generation replayed with two accounts and MaxRetries=3: ai=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}
