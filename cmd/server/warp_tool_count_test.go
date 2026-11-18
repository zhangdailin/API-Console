package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/config"
	"orchids-api/internal/handler"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

// Keep even the largest historical fixture small so these cases isolate tool
// cardinality, independently of cumulative schema size.
func warpToolCountDefinitions(t *testing.T, count int) []map[string]any {
	t.Helper()
	tools := make([]map[string]any, count)
	schemaBytes := 0
	for i := range tools {
		schema := map[string]any{
			"type": "object", "description": fmt.Sprintf("s%03d", i),
			"properties": map[string]any{
				"q": map[string]any{"type": "string", "description": fmt.Sprintf("q%03d", i), "minLength": i % 3},
			},
			"required": []string{"q"}, "additionalProperties": false,
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		schemaBytes += len(raw)
		tools[i] = map[string]any{
			"name": fmt.Sprintf("count_tool_%03d", i), "description": fmt.Sprintf("Tool %03d.", i),
			"input_schema": schema,
		}
	}
	if schemaBytes >= 64<<10 {
		t.Fatalf("historical small fixture schemas are not tiny: %d bytes", schemaBytes)
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) >= 64<<10 {
		t.Fatalf("fixture definitions are not tiny: %d bytes", len(raw))
	}
	// Match the original HTTP JSON's number and array types when comparing with
	// structpb.AsMap; comparing an int or []string would obscure real differences.
	var decoded []map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func warpToolCountBody(t *testing.T, tools []map[string]any, streaming bool) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model": warpRouteModel, "stream": streaming, "tools": tools,
		"messages": []map[string]string{{"role": "user", "content": warpRouteQuery}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Pad schema descriptions with ASCII so marshaled JSON sizes are exact while
// retaining distinct properties, required fields and numeric constraints.
func warpToolSchemaSizeDefinitions(t *testing.T, count, totalBytes int) []map[string]any {
	t.Helper()
	tools := warpToolCountDefinitions(t, count)
	actualTotal := 0
	for i, tool := range tools {
		wantBytes := totalBytes / count
		if i < totalBytes%count {
			wantBytes++
		}
		schema := tool["input_schema"].(map[string]any)
		raw, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		if wantBytes < len(raw) || wantBytes > 64<<10 {
			t.Fatalf("schema %d target size %d is outside valid fixture bounds", i, wantBytes)
		}
		schema["description"] = schema["description"].(string) + strings.Repeat("s", wantBytes-len(raw))
		raw, err = json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != wantBytes {
			t.Fatalf("schema %d = %d bytes, want %d", i, len(raw), wantBytes)
		}
		actualTotal += len(raw)
	}
	if actualTotal != totalBytes || actualTotal <= 64<<10 {
		t.Fatalf("cumulative schemas = %d bytes, want %d above 64KiB", actualTotal, totalBytes)
	}
	return tools
}

func warpAssertMessagesToolsReachMCPUnchanged(t *testing.T, prefix string, streaming bool, want []map[string]any) {
	t.Helper()
	count := len(want)
	stub := newWarpRouteStub(t, http.StatusOK)
	e, s, h := newWarpRouteE2E(t, stub, 1)
	payloads := make(chan upstream.UpstreamRequest, 2)
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) handler.UpstreamClient {
		spec, ok := provider.Lookup(acc.AccountType)
		if !ok || spec.NewClient == nil {
			t.Error("Warp provider is not registered")
			return nil
		}
		return &warpRoutePayloadObserver{next: spec.NewClient(acc, cfg, s), payloads: payloads}
	})
	resp := e.do(t, http.MethodPost, prefix+"/messages", warpToolCountBody(t, want, streaming), false)
	out := e.readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(out, warpRouteAnswer) {
		t.Fatalf("Messages with %d tools = %d %s", count, resp.StatusCode, out)
	}
	if streaming {
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !strings.Contains(out, "event: message_stop") {
			t.Fatalf("invalid native Messages SSE: %s", out)
		}
	} else if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Fatalf("invalid native Messages response: %s", out)
	}
	if stub.aiCalls.Load() != 1 || stub.otherCalls.Load() != 0 {
		t.Fatalf("upstream calls: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
	}
	select {
	case payload := <-payloads:
		if payload.Model != warpRouteModel || len(payload.Tools) != count || len(payload.Messages) != 1 || payload.Messages[0].Content.Text != warpRouteQuery {
			t.Fatal("shared pipeline changed the model, native input text or tool count")
		}
	default:
		t.Fatal("Messages request bypassed the shared provider pipeline")
	}
	select {
	case raw := <-stub.requests:
		var request warpapi.Request
		if err := proto.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		inputs := request.GetInput().GetUserInputs().GetInputs()
		if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != warpRouteQuery {
			t.Fatal("tool declarations changed or augmented native input text")
		}
		mcp := request.GetMcpContext()
		servers := mcp.GetServers()
		if len(mcp.GetTools()) != 0 || len(servers) != 1 || servers[0].GetName() != "client" || servers[0].GetId() != "client-request-tools" {
			t.Fatal("tools did not retain their single client MCP server group")
		}
		tools := servers[0].GetTools()
		if len(tools) != count {
			t.Fatalf("protobuf MCP tools = %d, want %d including final tool %q", len(tools), count, want[count-1]["name"])
		}
		for i, tool := range tools {
			if tool.GetName() != want[i]["name"] || tool.GetDescription() != want[i]["description"] {
				t.Fatalf("tool %d name, order or description changed: got %q / %q", i, tool.GetName(), tool.GetDescription())
			}
			if tool.GetInputSchema() == nil || !reflect.DeepEqual(tool.GetInputSchema().AsMap(), want[i]["input_schema"]) {
				t.Fatalf("tool %d full schema changed: got %#v, want %#v", i, tool.GetInputSchema().AsMap(), want[i]["input_schema"])
			}
		}
	default:
		t.Fatal("Messages request did not reach mock generation")
	}
	if len(payloads) != 0 || len(stub.requests) != 0 {
		t.Fatal("one client request produced multiple upstream payloads")
	}
}

func TestWarpMessagesToolCountsAbove64ReachMCPUnchanged(t *testing.T) {
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			for _, count := range []int{78, 128, 256} {
				t.Run(fmt.Sprintf("%s/stream=%t/tools=%d", prefix, streaming, count), func(t *testing.T) {
					warpAssertMessagesToolsReachMCPUnchanged(t, prefix, streaming, warpToolCountDefinitions(t, count))
				})
			}
		}
	}
}

func TestWarpMessagesCumulativeToolSchemasAbove64KiBReachMCPUnchanged(t *testing.T) {
	cases := []struct {
		name       string
		count      int
		totalBytes int
	}{
		{"78_tools_1KiB_each", 78, 78 * 1024},
		{"48_tools_exact_65855_bytes", 48, 65855},
		{"128KiB_cumulative", 78, 128 << 10},
		{"256KiB_cumulative", 78, 256 << 10},
	}
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", prefix, streaming, tc.name), func(t *testing.T) {
					tools := warpToolSchemaSizeDefinitions(t, tc.count, tc.totalBytes)
					warpAssertMessagesToolsReachMCPUnchanged(t, prefix, streaming, tools)
				})
			}
		}
	}
}

func TestWarpMessagesToolValidationBeyond64StillRejectsBeforeGeneration(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]map[string]any)
	}{
		{"malformed_tool", func(tools []map[string]any) { tools[77] = nil }},
		{"duplicate_name", func(tools []map[string]any) { tools[77]["name"] = tools[0]["name"] }},
		{"case_fold_duplicate", func(tools []map[string]any) { tools[77]["name"] = strings.ToUpper(tools[0]["name"].(string)) }},
		{"unicode_equal_fold_duplicate", func(tools []map[string]any) {
			tools[0]["name"], tools[77]["name"] = "kelvin", "\u212aELVIN"
		}},
		{"invalid_schema", func(tools []map[string]any) { tools[77]["input_schema"] = []any{} }},
		{"description_over_32KiB", func(tools []map[string]any) { tools[77]["description"] = strings.Repeat("d", (32<<10)+1) }},
		{"schema_over_64KiB", func(tools []map[string]any) {
			tools[77]["input_schema"].(map[string]any)["description"] = strings.Repeat("s", 64<<10)
		}},
	}
	for _, prefix := range []string{"/warp", "/warp/v1"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range cases {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", prefix, streaming, tc.name), func(t *testing.T) {
					stub := newWarpRouteStub(t, http.StatusOK)
					e, _, _ := newWarpRouteE2E(t, stub, 1)
					tools := warpToolCountDefinitions(t, 78)
					tc.mutate(tools)
					resp := e.do(t, http.MethodPost, prefix+"/messages", warpToolCountBody(t, tools, streaming), false)
					out := e.readBody(t, resp)
					if resp.StatusCode != http.StatusBadRequest {
						t.Fatalf("invalid tool beyond index 64 = %d %s", resp.StatusCode, out)
					}
					if stub.aiCalls.Load() != 0 || stub.otherCalls.Load() != 0 || len(stub.requests) != 0 {
						t.Fatalf("invalid request reached upstream: generation=%d other=%d", stub.aiCalls.Load(), stub.otherCalls.Load())
					}
				})
			}
		}
	}
}
