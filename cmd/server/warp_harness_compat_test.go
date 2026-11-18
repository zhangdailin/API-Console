package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Reproduce only the reported client shapes using synthetic instructions,
// context and tools; never retain a production request or send a real generation.
func TestWarpHarnessCompatibilityRoutes(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, shape := range []string{"chat_minimal", "chat_context_tools", "messages_eager_tools"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", prefix, shape, stream), func(t *testing.T) {
					stub := newWarpUsageRouteStub(t, warpapi.ResponseEvent_StreamFinished_builder{RequestCharges: warpUsageDistinctCharges()}.Build(), nil)
					e, _, _ := newWarpRouteE2E(t, stub, 1)
					instructions := "Synthetic developer instructions."
					context := ""
					messages := []map[string]any{{"role": "developer", "content": instructions}}
					body := map[string]any{"model": warpRouteModel, "stream": stream, "store": false, "stream_options": map[string]any{"include_usage": true}, "max_completion_tokens": 128000}
					path := prefix + "/chat/completions"
					if shape != "chat_minimal" {
						instructions = strings.Repeat("s", 29079)
						messages[0]["content"] = instructions
						context = "Hi\n\n" + strings.Repeat("c", 29959) + "\n\n" + strings.Repeat("e", 932) + "\n\n"
						messages = append(messages, map[string]any{"role": "user", "content": "Hi"}, map[string]any{"role": "user", "content": strings.Repeat("c", 29959)}, map[string]any{"role": "user", "content": strings.Repeat("e", 932)})
						tools := make([]map[string]any, 111)
						for i := range tools {
							fn := map[string]any{"name": fmt.Sprintf("synthetic_tool_%03d", i), "description": "Synthetic client tool.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string"}}, "required": []string{"question"}}}
							tools[i] = map[string]any{"type": "function", "function": fn}
							if shape == "messages_eager_tools" {
								tools[i] = map[string]any{"name": fn["name"], "description": fn["description"], "input_schema": fn["parameters"], "eager_input_streaming": i%2 == 0}
							}
						}
						body["tools"] = tools
					}
					if shape == "messages_eager_tools" {
						path = prefix + "/messages"
						messages = messages[1:]
						body["system"] = instructions
						body["max_tokens"] = 128000
						delete(body, "store")
						delete(body, "stream_options")
						delete(body, "max_completion_tokens")
					}
					messages = append(messages, map[string]any{"role": "user", "content": warpRouteQuery})
					body["messages"] = messages
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					response := e.do(t, http.MethodPost, path, string(raw), false)
					out := e.readBody(t, response)
					if response.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
						t.Fatalf("client compatibility request = %d %s", response.StatusCode, out)
					}
					protocol := "chat/completions"
					if shape == "messages_eager_tools" {
						protocol = "messages"
					}
					usage := warpUsageFromWire(t, protocol, stream, out)
					if protocol == "messages" {
						warpUsageWantCounter(t, usage, "input_tokens", 130)
					} else {
						warpUsageWantCounter(t, usage, "prompt_tokens", 27086)
					}
					if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
						t.Fatalf("unexpected upstream calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
					}
					wire := <-stub.requests
					var request warpapi.Request
					if err := proto.Unmarshal(wire, &request); err != nil {
						t.Fatal(err)
					}
					inputs := request.GetInput().GetUserInputs().GetInputs()
					want := "System instructions:\n" + instructions + "\n\nUser: " + context + warpRouteQuery
					if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != want || request.GetSettings().GetModelConfig().GetBase() != warpRouteModel {
						t.Fatal("instructions, user context, or exact model changed")
					}
					var declared int
					for _, server := range request.GetMcpContext().GetServers() {
						for _, tool := range server.GetTools() {
							if tool.GetName() != fmt.Sprintf("synthetic_tool_%03d", declared) || tool.GetDescription() != "Synthetic client tool." {
								t.Fatal("tool order, name, or description changed")
							}
							declared++
						}
					}
					wantTools := 111
					if shape == "chat_minimal" {
						wantTools = 0
					}
					if declared != wantTools {
						t.Fatalf("declared tools = %d, want %d", declared, wantTools)
					}
					serialized, err := protojson.Marshal(&request)
					if err != nil {
						t.Fatal(err)
					}
					for _, hint := range []string{"eager_input_streaming", "include_usage", "stream_options"} {
						if strings.Contains(string(serialized), hint) {
							t.Fatalf("client hint %s reached the Warp protocol", hint)
						}
					}
				})
			}
		}
	}
}

func TestWarpClientCompatibilityMalformedHintsRejectedBeforeUpstream(t *testing.T) {
	stub := newWarpRouteStub(t, http.StatusOK)
	e, _, _ := newWarpRouteE2E(t, stub, 1)
	for _, fields := range []map[string]any{
		{"store": true}, {"store": nil}, {"store": "false"},
		{"stream_options": nil}, {"stream_options": map[string]any{"include_usage": "true"}},
		{"stream_options": map[string]any{"unknown-private-option": false}},
		{"tools": []any{map[string]any{"name": "echo", "input_schema": map[string]any{}, "eager_input_streaming": nil}}},
		{"tools": []any{map[string]any{"name": "echo", "input_schema": map[string]any{}, "eager_input_streaming": "true"}}},
		{"messages": []any{map[string]any{"role": "developer", "content": []any{map[string]any{"type": "tool_use", "id": "unbound", "name": "echo", "input": map[string]any{}}}}, map[string]any{"role": "user", "content": "hi"}}},
	} {
		body := map[string]any{"model": warpRouteModel, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		for k, v := range fields {
			body[k] = v
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/warp/v1/chat/completions", "/warp/v1/messages"} {
			response := e.do(t, http.MethodPost, path, string(raw), false)
			out := e.readBody(t, response)
			if response.StatusCode != http.StatusBadRequest || strings.Contains(out, "unknown-private-option") {
				t.Fatalf("invalid hint was accepted or leaked: %d %s", response.StatusCode, out)
			}
		}
	}
	if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 {
		t.Fatal("invalid hints reached credential refresh or generation")
	}
}
