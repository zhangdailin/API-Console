package main

import (
	"encoding/json"
	"net/http"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func TestWarpMessagesAcceptsAnthropicEffortAndThinkingWithTools(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, s, h := newWarpRouteE2E(t, stub, 1)

	// Observe the shared UpstreamRequest to prove the effort hint was accepted
	// without any model-variant rewrite and without forwarding effort controls.
	payloads := make(chan upstream.UpstreamRequest, 4)
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
		spec, ok := provider.Lookup(acc.AccountType)
		if !ok || spec.NewClient == nil {
			t.Error("Warp provider is not registered")
			return nil
		}
		return &warpRoutePayloadObserver{next: spec.NewClient(acc, cfg, s), payloads: payloads}
	})

	tools := make([]map[string]any, 0, 16)
	for i := 0; i < 16; i++ {
		tools = append(tools, map[string]any{
			"name":        "tool_" + string(rune('a'+i)),
			"description": "client tool",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q": map[string]any{"type": "string"},
				},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"model": warpRouteModel,
		"messages": []map[string]any{
			{"role": "user", "content": warpRouteQuery},
		},
		"stream":        false,
		"max_tokens":    64,
		"output_config": map[string]any{"effort": "high"},
		"thinking":      map[string]any{"type": "adaptive", "display": nil},
		"metadata":      map[string]any{"user_id": "synthetic-user"},
		"tools":         tools,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The real /warp/v1/messages route must accept the Anthropic shape and
	// return a 200 from the mock Warp upstream; no real model is contacted.
	resp := e.do(t, http.MethodPost, "/warp/v1/messages", string(body), false)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/warp/v1/messages with Anthropic effort controls = %d %s", resp.StatusCode, out)
	}
	var message struct {
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
	if message.Model != warpRouteModel || message.StopReason != "end_turn" || len(message.Content) != 1 || message.Content[0].Text != warpRouteAnswer {
		t.Fatalf("unexpected Anthropic response: %s", out)
	}

	select {
	case payload := <-payloads:
		// ExactModelID forbids effort-suffixed rewrites: the model that reaches
		// the Warp client is unchanged even though ReasoningEffort is non-empty.
		if payload.Model != warpRouteModel || payload.ReasoningEffort == "" {
			t.Fatalf("model/effort forwarding wrong: model=%q effort=%q", payload.Model, payload.ReasoningEffort)
		}
		if len(payload.Tools) != 16 {
			t.Fatalf("client tools dropped: got %d, want 16", len(payload.Tools))
		}
		if payload.ResponseFormat != nil || payload.ResponseText != nil || len(payload.ResponsesTools) != 0 {
			t.Fatal("unsupported response/tool controls reached the upstream request")
		}
	default:
		t.Fatal("request bypassed the shared Warp provider pipeline")
	}

	select {
	case raw := <-stub.requests:
		var request warpapi.Request
		if err := proto.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		settings := request.GetSettings()
		if got := settings.GetModelConfig().GetBase(); got != warpRouteModel || settings.GetModelConfig().HasCliAgent() || settings.GetModelConfig().HasCoding() || settings.GetModelConfig().HasPlanning() {
			t.Fatalf("effort rewrote exact model or added a subagent: %q", got)
		}
		if settings.GetCustomModelProviders() != nil {
			t.Fatal("no reasoning_effort provider may be configured")
		}
		if settings.HasSupportsReasoningMessage() && settings.GetSupportsReasoningMessage() {
			t.Fatal("reasoning-message capability must stay false")
		}
		if request.GetMetadata().GetConversationId() != "" {
			t.Fatal("user_id metadata must not become an upstream conversation id")
		}
		var declared int
		for _, server := range request.GetMcpContext().GetServers() {
			declared += len(server.GetTools())
		}
		if declared != 16 {
			t.Fatalf("Warp declared %d MCP tools, want 16", declared)
		}
		inputs := request.GetInput().GetUserInputs().GetInputs()
		if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != warpRouteQuery {
			t.Fatal("Warp did not send exactly the original user text")
		}
	default:
		t.Fatal("Anthropic effort request never reached mock generation")
	}
	if stub.otherCalls.Load() != 0 {
		t.Fatalf("unexpected non-generation upstream calls: %d", stub.otherCalls.Load())
	}
}
