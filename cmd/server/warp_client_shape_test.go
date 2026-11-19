package main

import (
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

// Every byte here is generated test data. The fixture reproduces client request
// sizes and field shapes without retaining any user instructions or tool data.
type warpClientShapeFixture struct {
	body        map[string]any
	system      []string
	user        string
	tools       []map[string]any
	descBytes   int
	schemaBytes int
	toolBytes   int
}

func warpClientShapeText(label string, size int) string {
	return label + strings.Repeat("x", size-len(label)-len("-end")) + "-end"
}

func newWarpClientShapeFixture(t *testing.T, streaming, display bool) warpClientShapeFixture {
	t.Helper()
	f := warpClientShapeFixture{
		system: []string{
			warpClientShapeText("synthetic-top-a-", 3000),
			warpClientShapeText("synthetic-top-b-", 4000),
			warpClientShapeText("synthetic-top-c-", 4090),
			warpClientShapeText("synthetic-trailing-system-", 13233),
		},
		user: warpClientShapeText("synthetic-user-", 581),
	}
	blocks := make([]map[string]any, 3)
	for i := range blocks {
		blocks[i] = map[string]any{"type": "text", "text": f.system[i]}
		if i != 0 {
			blocks[i]["cache_control"] = map[string]any{"type": "ephemeral"}
		}
	}
	for i := 0; i < 29; i++ {
		descSize := 2177
		if i == 0 {
			descSize = 20231
		} else if i == 28 {
			descSize = 2194
		}
		schemaSize := 1679
		if i == 28 {
			schemaSize = 1699
		}
		schema := map[string]any{
			"type":       "object",
			"properties": map[string]any{"query": map[string]any{"type": "string", "description": ""}},
			"required":   []string{"query"}, "additionalProperties": false,
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		schema["properties"].(map[string]any)["query"].(map[string]any)["description"] = warpClientShapeText("synthetic-schema-", schemaSize-len(raw))
		tool := map[string]any{
			"name":         fmt.Sprintf("synthetic_tool_%02d", i),
			"description":  warpClientShapeText(fmt.Sprintf("synthetic-description-%02d-", i), descSize),
			"input_schema": schema,
		}
		raw, err = json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		f.schemaBytes += len(raw)
		f.descBytes += descSize
		raw, err = json.Marshal(tool)
		if err != nil {
			t.Fatal(err)
		}
		f.toolBytes += len(raw)
		f.tools = append(f.tools, tool)
	}
	if len(f.system[0])+len(f.system[1])+len(f.system[2]) != 11090 || len(f.system[3]) != 13233 || len(f.user) != 581 || len(f.tools) != 29 {
		t.Fatal("synthetic client fixture no longer matches the expected content sizes")
	}
	// Keep the original client-shape fixture below the former complete-definition cap.
	if f.descBytes != 81204 || len(f.tools[0]["description"].(string)) != 20231 || f.schemaBytes != 48711 || f.toolBytes > 160<<10 {
		t.Fatalf("synthetic fixture sizes drifted from the original below-old-cap shape: descriptions=%d schemas=%d definitions=%d", f.descBytes, f.schemaBytes, f.toolBytes)
	}
	thinking := map[string]any{"type": "adaptive"}
	if display {
		thinking["display"] = "omitted"
	}
	f.body = map[string]any{
		"model": warpRouteModel, "system": blocks,
		"messages": []map[string]any{
			{"role": "user", "content": f.user},
			{"role": "system", "content": []map[string]any{{"type": "text", "text": f.system[3], "cache_control": map[string]any{"type": "ephemeral"}}}},
		},
		"tools": f.tools, "max_tokens": 1024, "stream": streaming,
		"context_management": map[string]any{"edits": []map[string]any{{"type": "clear_thinking_20251015", "keep": "all"}}},
		"thinking":           thinking, "output_config": map[string]any{"effort": "high"},
		"metadata": map[string]any{"user_id": "synthetic-client-shape-user"},
	}
	return f
}

func (f warpClientShapeFixture) marshal(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(f.body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestWarpNativeMessagesSyntheticClientShape(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			for _, display := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/display=%t", prefix, streaming, display), func(t *testing.T) {
					// Each case owns its listeners, account store and call counters.
					stub := newWarpRouteStub(t, http.StatusOK)
					e, s, h := newWarpRouteE2E(t, stub, 1)
					payloads := make(chan upstream.UpstreamRequest, 2)
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
					f := newWarpClientShapeFixture(t, streaming, display)
					beforeAI, beforeOther, beforeClients := stub.aiCalls.Load(), stub.otherCalls.Load(), clients.Load()
					resp := e.do(t, http.MethodPost, prefix+"/messages", f.marshal(t), false)
					out := e.readBody(t, resp)
					if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) || !strings.Contains(out, `"model":"`+warpRouteModel+`"`) {
						t.Fatalf("native Messages client shape = %d %s", resp.StatusCode, out)
					}
					if streaming {
						if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, "event: message_stop") {
							t.Fatalf("invalid native Messages SSE: %s", out)
						}
					} else if !strings.Contains(out, `"stop_reason":"end_turn"`) {
						t.Fatalf("invalid native Messages response: %s", out)
					}
					if stub.aiCalls.Load()-beforeAI != 1 || stub.otherCalls.Load()-beforeOther != 0 || clients.Load()-beforeClients != 1 {
						t.Fatalf("request call deltas: generation=%d other=%d clients=%d", stub.aiCalls.Load()-beforeAI, stub.otherCalls.Load()-beforeOther, clients.Load()-beforeClients)
					}
					select {
					case payload := <-payloads:
						if payload.Model != warpRouteModel || len(payload.Tools) != 29 {
							t.Fatal("shared pipeline changed model identity or lost client tools")
						}
					default:
						t.Fatal("native Messages request bypassed the shared pipeline")
					}
					select {
					case raw := <-stub.requests:
						assertWarpClientShapeWire(t, raw, f)
					default:
						t.Fatal("native Messages request did not reach the isolated upstream")
					}
					if len(payloads) != 0 || len(stub.requests) != 0 {
						t.Fatal("one client request produced multiple upstream payloads")
					}
				})
			}
		}
	}
}

func assertWarpClientShapeWire(t *testing.T, raw []byte, f warpClientShapeFixture) {
	t.Helper()
	var request warpapi.Request
	if err := proto.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	model := request.GetSettings().GetModelConfig()
	if model.GetBase() != warpRouteModel || model.HasCliAgent() || model.HasComputerUseAgent() || model.HasCoding() || model.HasPlanning() {
		t.Fatal("advisory controls changed the exact upstream model")
	}
	inputs := request.GetInput().GetUserInputs().GetInputs()
	wantQuery := "System instructions:\n" + strings.Join(f.system, "\n") + "\n\nUser: " + f.user
	if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != wantQuery {
		t.Fatal("prepared query dropped, truncated, reordered or augmented system/user text")
	}
	if request.GetInput().GetContext().HasCurrentTime() || request.GetMetadata().GetConversationId() != "" || len(request.GetTaskContext().GetTasks()) != 0 {
		t.Fatal("client hints introduced timestamps, conversation identity or task context")
	}
	servers := request.GetMcpContext().GetServers()
	if len(servers) != 1 || servers[0].GetName() != "client" || servers[0].GetId() != warpRouteMCPServerID || len(servers[0].GetTools()) != 29 {
		t.Fatal("client tool declarations did not retain their single 29-tool server group")
	}
	for i, tool := range servers[0].GetTools() {
		want := f.tools[i]
		if tool.GetName() != want["name"] || tool.GetDescription() != want["description"] {
			t.Fatalf("tool %d name/description changed or truncated", i)
		}
		gotSchema, err := json.Marshal(tool.GetInputSchema().AsMap())
		if err != nil {
			t.Fatal(err)
		}
		wantSchema, err := json.Marshal(want["input_schema"])
		if err != nil {
			t.Fatal(err)
		}
		if string(gotSchema) != string(wantSchema) {
			t.Fatalf("tool %d schema changed or truncated: got_bytes=%d want_bytes=%d", i, len(gotSchema), len(wantSchema))
		}
	}
	wire, err := protojson.Marshal(&request)
	if err != nil {
		t.Fatal(err)
	}
	for _, hint := range []string{"cache_control", "cacheControl", "ephemeral", "context_management", "clear_thinking_20251015", "classifier_context", "adaptive", "omitted", "output_config", "synthetic-client-shape-user"} {
		if strings.Contains(string(wire), hint) {
			t.Fatalf("client-only hint %q leaked into Warp protobuf", hint)
		}
	}
	settings := request.GetSettings()
	if settings.GetCustomModelProviders() != nil || settings.GetSupportsReasoningMessage() || len(settings.GetSupportedTools()) != 1 || settings.GetSupportedTools()[0] != warpapi.ToolType_CALL_MCP_TOOL {
		t.Fatal("compatibility hints enabled unsupported providers, reasoning or server tools")
	}
}

func TestWarpNativeMessagesClientSafeguardsRejectedBeforeClientConstruction(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			for _, context := range []struct {
				name  string
				value map[string]any
			}{
				{"empty_classifier_context", map[string]any{}},
				{"nonempty_classifier_context", map[string]any{"category": "synthetic", "enabled": true}},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", prefix, streaming, context.name), func(t *testing.T) {
					stub := newWarpRouteStub(t, http.StatusOK)
					e, s, h := newWarpRouteE2E(t, stub, 1)
					var clients atomic.Int64
					h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
						clients.Add(1)
						spec, _ := provider.Lookup(acc.AccountType)
						return spec.NewClient(acc, cfg, s)
					})
					f := newWarpClientShapeFixture(t, streaming, true)
					f.body["safeguards"] = []map[string]any{{"type": "some_client_safeguard", "classifier_context": context.value}}
					beforeAI, beforeOther, beforeClients := stub.aiCalls.Load(), stub.otherCalls.Load(), clients.Load()
					resp := e.do(t, http.MethodPost, prefix+"/messages", f.marshal(t), false)
					out := e.readBody(t, resp)
					if resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "invalid_request_error") || !strings.Contains(out, "safeguards") {
						t.Fatalf("nonempty safeguards must fail specifically: %d %s", resp.StatusCode, out)
					}
					if clients.Load()-beforeClients != 0 || stub.aiCalls.Load()-beforeAI != 0 || stub.otherCalls.Load()-beforeOther != 0 {
						t.Fatalf("safeguards reached account client/generation/refresh: clients=%d generation=%d other=%d", clients.Load()-beforeClients, stub.aiCalls.Load()-beforeAI, stub.otherCalls.Load()-beforeOther)
					}
				})
			}
		}
	}
}

func TestWarpNativeMessagesClientShapeDefinitionsOverFormer160KiBCap(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", prefix, streaming), func(t *testing.T) {
				stub := newWarpRouteStub(t, http.StatusOK)
				e, s, h := newWarpRouteE2E(t, stub, 1)
				var clients atomic.Int64
				h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
					clients.Add(1)
					spec, _ := provider.Lookup(acc.AccountType)
					return spec.NewClient(acc, cfg, s)
				})
				f := newWarpClientShapeFixture(t, streaming, false)
				f.descBytes, f.schemaBytes, f.toolBytes = 0, 0, 0
				for i, tool := range f.tools {
					// Only the removed cumulative definition cap is exceeded:
					// each description and the aggregate schemas remain bounded.
					tool["description"] = warpClientShapeText(fmt.Sprintf("synthetic-large-definition-%02d-", i), 6000)
					descBytes := len(tool["description"].(string))
					if descBytes >= 32<<10 {
						t.Fatalf("tool %d description must remain below 32 KiB: %d", i, descBytes)
					}
					f.descBytes += descBytes
					raw, err := json.Marshal(tool["input_schema"])
					if err != nil {
						t.Fatal(err)
					}
					f.schemaBytes += len(raw)
					raw, err = json.Marshal(tool)
					if err != nil {
						t.Fatal(err)
					}
					f.toolBytes += len(raw)
				}
				if len(f.tools) != 29 || len(f.tools) > 64 || f.schemaBytes >= 64<<10 || f.toolBytes <= 160<<10 || f.toolBytes < 164060 {
					t.Fatalf("large-definition fixture must exceed the former cap within retained limits: tools=%d descriptions=%d schemas=%d definitions=%d", len(f.tools), f.descBytes, f.schemaBytes, f.toolBytes)
				}
				beforeAI, beforeOther, beforeClients := stub.aiCalls.Load(), stub.otherCalls.Load(), clients.Load()
				resp := e.do(t, http.MethodPost, prefix+"/messages", f.marshal(t), false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) || !strings.Contains(out, `"model":"`+warpRouteModel+`"`) {
					t.Fatalf("complete tool definitions above the former cap must succeed: %d %s", resp.StatusCode, out)
				}
				if streaming {
					if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, "event: message_stop") {
						t.Fatalf("invalid native Messages SSE: %s", out)
					}
				} else if !strings.Contains(out, `"stop_reason":"end_turn"`) {
					t.Fatalf("invalid native Messages response: %s", out)
				}
				if stub.aiCalls.Load()-beforeAI != 1 || stub.otherCalls.Load()-beforeOther != 0 || clients.Load()-beforeClients != 1 {
					t.Fatalf("request call deltas: generation=%d other=%d clients=%d", stub.aiCalls.Load()-beforeAI, stub.otherCalls.Load()-beforeOther, clients.Load()-beforeClients)
				}
				select {
				case raw := <-stub.requests:
					assertWarpClientShapeWire(t, raw, f)
				default:
					t.Fatal("large tool definitions did not reach the isolated upstream")
				}
				if len(stub.requests) != 0 {
					t.Fatal("one client request produced multiple upstream payloads")
				}
			})
		}
	}
}

func TestWarpNativeMessagesClientShapeLargeSystemPreservesSourceOrder(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			for _, mergedSystemBytes := range []int{63712, 128 << 10, 256 << 10} {
				t.Run(fmt.Sprintf("%s/stream=%t/system_bytes=%d", prefix, streaming, mergedSystemBytes), func(t *testing.T) {
					stub := newWarpRouteStub(t, http.StatusOK)
					e, s, h := newWarpRouteE2E(t, stub, 1)
					var clients atomic.Int64
					h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
						clients.Add(1)
						spec, _ := provider.Lookup(acc.AccountType)
						return spec.NewClient(acc, cfg, s)
					})
					f := newWarpClientShapeFixture(t, streaming, false)
					// Grow only the first block, retaining the source order of all three
					// top-level blocks and the system message after the user message.
					f.system[0] = warpClientShapeText("synthetic-large-system-", mergedSystemBytes-len(f.system[1])-len(f.system[2])-len(f.system[3])-3)
					f.body["system"].([]map[string]any)[0]["text"] = f.system[0]
					if got := len(strings.Join(f.system, "\n")); got != mergedSystemBytes {
						t.Fatalf("merged system fixture bytes = %d, want %d", got, mergedSystemBytes)
					}
					beforeAI, beforeOther, beforeClients := stub.aiCalls.Load(), stub.otherCalls.Load(), clients.Load()
					resp := e.do(t, http.MethodPost, prefix+"/messages", f.marshal(t), false)
					out := e.readBody(t, resp)
					if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) || !strings.Contains(out, `"model":"`+warpRouteModel+`"`) {
						t.Fatalf("large system instructions must succeed intact: %d %s", resp.StatusCode, out)
					}
					if streaming {
						if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, "event: message_stop") {
							t.Fatalf("invalid native Messages SSE: %s", out)
						}
					} else if !strings.Contains(out, `"stop_reason":"end_turn"`) {
						t.Fatalf("invalid native Messages response: %s", out)
					}
					if stub.aiCalls.Load()-beforeAI != 1 || stub.otherCalls.Load()-beforeOther != 0 || clients.Load()-beforeClients != 1 {
						t.Fatalf("request call deltas: generation=%d other=%d clients=%d", stub.aiCalls.Load()-beforeAI, stub.otherCalls.Load()-beforeOther, clients.Load()-beforeClients)
					}
					select {
					case raw := <-stub.requests:
						assertWarpClientShapeWire(t, raw, f)
					default:
						t.Fatal("large system instructions did not reach the isolated upstream")
					}
					if len(stub.requests) != 0 {
						t.Fatal("one client request produced multiple upstream payloads")
					}
				})
			}
		}
	}
}

func TestWarpNativeMessagesClientShapeAboveFormerToolBudget(t *testing.T) {
	cases := []struct {
		name, code, path string
		mutate           func(*warpClientShapeFixture)
	}{
		{"description_over_32KiB", "tool_description_bytes", "tools[0].description", func(f *warpClientShapeFixture) {
			f.tools[0]["description"] = warpClientShapeText("synthetic-oversized-description-", (32<<10)+1)
		}},
	}
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, tc := range cases {
			t.Run(prefix+"/"+tc.name, func(t *testing.T) {
				stub := newWarpRouteStub(t, http.StatusOK)
				e, s, h := newWarpRouteE2E(t, stub, 1)
				var clients atomic.Int64
				h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
					clients.Add(1)
					spec, _ := provider.Lookup(acc.AccountType)
					return spec.NewClient(acc, cfg, s)
				})
				f := newWarpClientShapeFixture(t, false, false)
				tc.mutate(&f)
				beforeAI, beforeOther, beforeClients := stub.aiCalls.Load(), stub.otherCalls.Load(), clients.Load()
				resp := e.do(t, http.MethodPost, prefix+"/messages", f.marshal(t), false)
				out := e.readBody(t, resp)
				if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
					t.Fatalf("large tool description rejected: %d %s", resp.StatusCode, out)
				}
				if strings.Contains(out, "synthetic-oversized-") {
					t.Fatal("tool description escaped into the answer")
				}
				if clients.Load()-beforeClients != 1 || stub.aiCalls.Load()-beforeAI != 1 || stub.otherCalls.Load()-beforeOther != 0 {
					t.Fatalf("large input call counts: clients=%d generation=%d other=%d", clients.Load()-beforeClients, stub.aiCalls.Load()-beforeAI, stub.otherCalls.Load()-beforeOther)
				}
				select {
				case raw := <-stub.requests:
					assertWarpClientShapeWire(t, raw, f)
				default:
					t.Fatal("large tool description did not reach the isolated upstream")
				}
			})
		}
	}
}
