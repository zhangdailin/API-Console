package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

func warpHistoryThinkingBody(t *testing.T, messages []map[string]any, tools []map[string]any) string {
	t.Helper()
	request := map[string]any{"model": warpRouteModel, "max_tokens": 64, "messages": messages}
	if tools != nil {
		request["tools"] = tools
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func warpHistoryThinkingTools() []map[string]any {
	tools := make([]map[string]any, 30)
	for i := range tools {
		tools[i] = map[string]any{
			"name": fmt.Sprintf("history_tool_%02d", i), "description": "Synthetic client tool",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		}
	}
	return tools
}

// The client may replay a real tool-call shape, but a replayed call id is not an
// authentic upstream binding. Thinking must not mask the existing 409 boundary.
func TestWarpMessagesHistoryThinkingToolResultRequiresBinding(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, s, h := newWarpRouteE2E(t, stub, 1)
	payloads := make(chan upstream.UpstreamRequest, 8)
	var clients atomic.Int64
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
		clients.Add(1)
		spec, ok := provider.Lookup(acc.AccountType)
		if !ok || spec.NewClient == nil {
			t.Error("Warp provider is not registered")
			return nil
		}
		return &warpRoutePayloadObserver{next: spec.NewClient(acc, cfg, s), payloads: payloads}
	})
	otherKey := "sk-warp-history-other-owner"
	digest := sha256.Sum256([]byte(otherKey))
	if err := s.CreateApiKey(context.Background(), &store.ApiKey{
		Name: "history-other-owner", KeyHash: hex.EncodeToString(digest[:]), KeyPrefix: "sk-", KeySuffix: "owner", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, trailingSystem := range []bool{false, true} {
		for _, key := range []string{e.managedKey, otherKey} {
			t.Run(fmt.Sprintf("trailing_system=%t/other_owner=%t", trailingSystem, key == otherKey), func(t *testing.T) {
				messages := []map[string]any{
					{"role": "user", "content": "First question."},
					{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "First answer."}}},
					{"role": "user", "content": "Inspect the synthetic input."},
					{"role": "assistant", "content": []map[string]any{
						{"type": "thinking", "thinking": "synthetic-private-tool-thought", "signature": "synthetic-private-tool-signature"},
						{"type": "tool_use", "id": "synthetic-unbound-call", "name": "history_tool_00", "input": map[string]any{"query": "synthetic input"}},
					}},
					{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "synthetic-unbound-call", "content": "Synthetic tool result."}}},
				}
				if trailingSystem {
					messages = append(messages, map[string]any{"role": "system", "content": []map[string]any{{"type": "text", "text": "Preserve these instructions."}}})
				}
				requester := *e
				requester.managedKey = key
				resp := requester.do(t, http.MethodPost, "/warp/v1/messages", warpHistoryThinkingBody(t, messages, warpHistoryThinkingTools()), false)
				out := e.readBody(t, resp)
				if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
					t.Fatal("unbound or other-owner continuation reached upstream")
				}
				if resp.StatusCode != http.StatusConflict {
					t.Errorf("unbound thinking/tool-result history = %d %s, want continuation 409 rather than thinking 400", resp.StatusCode, out)
				}
				if clients.Load() == 0 {
					t.Fatal("valid history never constructed a registered client")
				}
				select {
				case payload := <-payloads:
					if len(payload.Messages) != len(messages) || len(payload.Tools) != 30 {
						t.Fatal("shared pipeline dropped history or client tool declarations")
					}
					blocks := payload.Messages[3].Content.Blocks
					if len(blocks) != 2 || blocks[1].Type != "tool_use" || blocks[1].ID != "synthetic-unbound-call" || blocks[1].Name != "history_tool_00" {
						t.Fatal("thinking compatibility altered the adjacent tool-use block")
					}
				default:
					t.Fatal("history bypassed the shared provider pipeline")
				}
				if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
					t.Fatal("unbound or other-owner continuation reached upstream")
				}
			})
		}
	}
}

func TestWarpMessagesHistoryThinkingIgnoredOnUpstreamWire(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, signature := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/signature=%t", prefix, signature), func(t *testing.T) {
				thinking := map[string]any{"type": "thinking", "thinking": "synthetic-private-history-thought"}
				if signature {
					thinking["signature"] = "synthetic-private-history-signature"
				}
				messages := []map[string]any{
					{"role": "user", "content": "Historical question."},
					{"role": "assistant", "content": []map[string]any{
						{"type": "text", "text": "Historical answer before."}, thinking,
						{"type": "text", "text": "Historical answer after."},
					}},
					{"role": "user", "content": warpRouteQuery},
				}
				resp := e.do(t, http.MethodPost, prefix+"/messages", warpHistoryThinkingBody(t, messages, warpHistoryThinkingTools()), false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
					t.Fatalf("ordinary dialog with historical thinking = %d %s", resp.StatusCode, out)
				}
				if strings.Contains(out, "thinking") || strings.Contains(out, "signature") || strings.Contains(out, "synthetic-private") {
					t.Fatal("historical thinking or reasoning output leaked into the response")
				}
				select {
				case raw := <-stub.requests:
					var request warpapi.Request
					if err := proto.Unmarshal(raw, &request); err != nil {
						t.Fatal(err)
					}
					inputs := request.GetInput().GetUserInputs().GetInputs()
					want := "User: Historical question.\n\nAssistant: Historical answer before.\nAssistant: Historical answer after.\n\nUser: " + warpRouteQuery
					if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != want {
						t.Fatal("upstream query changed, lost or reordered non-thinking history text")
					}
					var count int
					for _, server := range request.GetMcpContext().GetServers() {
						count += len(server.GetTools())
					}
					if count != 30 {
						t.Fatalf("MCP tool declarations = %d, want 30", count)
					}
					wire, err := protojson.Marshal(&request)
					if err != nil {
						t.Fatal(err)
					}
					for _, forbidden := range []string{"synthetic-private-history-thought", "synthetic-private-history-signature", "\"thinking\"", "\"signature\""} {
						if strings.Contains(string(wire), forbidden) {
							t.Fatalf("historical thinking/signature reached the upstream wire: %q", forbidden)
						}
					}
					if request.GetSettings().GetSupportsReasoningMessage() {
						t.Fatal("historical thinking enabled reasoning-message output")
					}
				default:
					t.Fatal("accepted history never reached isolated generation")
				}
			})
		}
	}
	if stub.aiCalls.Load() != 4 || stub.otherCalls.Load() != 0 {
		t.Fatalf("upstream calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}

func TestWarpMessagesHistoryThinkingInvalidRawRejectedBeforeClient(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, s, h := newWarpRouteE2E(t, stub, 1)
	var clients atomic.Int64
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
		clients.Add(1)
		spec, ok := provider.Lookup(acc.AccountType)
		if !ok || spec.NewClient == nil {
			t.Error("Warp provider is not registered")
			return nil
		}
		return spec.NewClient(acc, cfg, s)
	})
	cases := map[string]string{
		"wrong_type":        `{"type":"Thinking","thinking":"synthetic"}`,
		"nonstring_type":    `{"type":7,"thinking":"synthetic"}`,
		"missing_thinking":  `{"type":"thinking"}`,
		"null_thinking":     `{"type":"thinking","thinking":null}`,
		"number_thinking":   `{"type":"thinking","thinking":1}`,
		"object_thinking":   `{"type":"thinking","thinking":{}}`,
		"array_thinking":    `{"type":"thinking","thinking":[]}`,
		"null_signature":    `{"type":"thinking","thinking":"synthetic","signature":null}`,
		"number_signature":  `{"type":"thinking","thinking":"synthetic","signature":1}`,
		"object_signature":  `{"type":"thinking","thinking":"synthetic","signature":{}}`,
		"unknown_field":     `{"type":"thinking","thinking":"synthetic","unexpected":null}`,
		"text_field":        `{"type":"thinking","thinking":"synthetic","text":""}`,
		"source_field":      `{"type":"thinking","thinking":"synthetic","source":null}`,
		"cache_control":     `{"type":"thinking","thinking":"synthetic","cache_control":{"type":"ephemeral"}}`,
		"redacted_thinking": `{"type":"redacted_thinking","data":"synthetic"}`,
		"image":             `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"synthetic"}}`,
	}
	bodies := make(map[string]string, len(cases)+6)
	for name, block := range cases {
		bodies[name] = `{"model":"` + warpRouteModel + `","max_tokens":64,"messages":[{"role":"user","content":"History."},{"role":"assistant","content":[{"type":"text","text":"Answer."},` + block + `]},{"role":"user","content":"Current."}]}`
	}
	bodies["user_role"] = `{"model":"` + warpRouteModel + `","messages":[{"role":"user","content":[{"type":"text","text":"Current."},{"type":"thinking","thinking":"synthetic"}]}]}`
	bodies["system_role"] = `{"model":"` + warpRouteModel + `","messages":[{"role":"system","content":[{"type":"thinking","thinking":"synthetic"}]},{"role":"user","content":"Current."}]}`
	bodies["malformed_json"] = `{"model":"` + warpRouteModel + `","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":}]}`
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for name, body := range bodies {
			t.Run(prefix+"/"+name, func(t *testing.T) {
				beforeClients, beforeAI, beforeOther := clients.Load(), stub.aiCalls.Load(), stub.otherCalls.Load()
				resp := e.do(t, http.MethodPost, prefix+"/messages", body, false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "invalid_request_error") {
					t.Errorf("invalid historical thinking = %d %s", resp.StatusCode, out)
				}
				if clients.Load() != beforeClients || stub.aiCalls.Load() != beforeAI || stub.otherCalls.Load() != beforeOther {
					t.Fatalf("invalid history reached client/generation/refresh: clients=%d generation=%d other=%d", clients.Load()-beforeClients, stub.aiCalls.Load()-beforeAI, stub.otherCalls.Load()-beforeOther)
				}
			})
		}
	}
}

func TestWarpMessagesHistoryThinkingOnlyAssistantRejected(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	for _, blankText := range []bool{false, true} {
		t.Run(fmt.Sprintf("blank_text=%t", blankText), func(t *testing.T) {
			blocks := []map[string]any{{"type": "thinking", "thinking": "synthetic-private-only-thought", "signature": "synthetic-private-only-signature"}}
			if blankText {
				blocks = append(blocks, map[string]any{"type": "text", "text": "   "})
			}
			messages := []map[string]any{
				{"role": "user", "content": "Historical question."},
				{"role": "assistant", "content": blocks},
				{"role": "user", "content": "Current question."},
			}
			resp := e.do(t, http.MethodPost, "/warp/v1/messages", warpHistoryThinkingBody(t, messages, nil), false)
			out := e.readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("assistant without visible content = %d %s, want 400", resp.StatusCode, out)
			}
			if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
				t.Fatal("assistant without visible content reached upstream")
			}
		})
	}
}

// Large historical provider metadata remains compatible, but is never replayed
// as visible dialog or as a transferable provider signature.
func TestWarpMessagesHistoryThinkingAboveFormerByteBudget(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	messages := []map[string]any{
		{"role": "user", "content": "First."},
		{"role": "assistant", "content": []map[string]any{{"type": "thinking", "thinking": strings.Repeat("界", (128<<10)/3) + "tt"}, {"type": "text", "text": "First answer."}}},
		{"role": "user", "content": "Second."},
		{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "Second answer."}, {"type": "thinking", "thinking": "", "signature": strings.Repeat("s", 128<<10)}}},
		{"role": "user", "content": "Current."},
	}
	resp := e.do(t, http.MethodPost, "/warp/v1/messages", warpHistoryThinkingBody(t, messages, nil), false)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
		t.Fatalf("exact 64 KiB historical thinking/signature budget = %d %s", resp.StatusCode, out)
	}
	if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
		t.Fatalf("boundary request calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
}
