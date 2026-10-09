package grok

import (
	"encoding/json"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"

	"orchids-api/internal/testutil"

	"testing"
)

func TestResponsesPayloadFromChatPreservesMultimodalAndNormalizesBuildState(t *testing.T) {
	req := &chatwire.Request{
		Model: "grok-4.5", PromptCacheKey: "session", SafetyIdentifier: "user-1",
		Messages: []chatwire.Message{{Role: "user", Content: []interface{}{
			map[string]interface{}{"type": "text", "text": "inspect"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AA=="}},
		}}},
	}
	payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.5"}, req, true)
	testutil.NoError(t, err)
	testutil.Equal(t, payload["prompt_cache_key"], "session")
	input := payload["input"].([]interface{})
	message := input[0].(map[string]interface{})
	parts := message["content"].([]interface{})
	testutil.Equal(t, len(parts), 2)
	testutil.Equal(t, parts[1].(map[string]interface{})["type"], "input_image")
	testutil.Equal(t, payload["safety_identifier"], "user-1")
}

func TestAnthropicRequestNormalizesMCPStrictAndOutputFormat(t *testing.T) {
	strict := true
	req := anthropicMessagesRequest{
		Model: "grok-4.6", MaxTokens: 1024, Messages: []anthropicMessage{{Role: "user", Content: "hello"}},
		Tools:        []anthropicTool{{Name: "read", InputSchema: map[string]interface{}{"type": "object"}, Strict: &strict}},
		MCPServers:   []map[string]interface{}{{"name": "docs", "url": "https://example.test/mcp", "authorization_token": "secret"}},
		OutputConfig: map[string]interface{}{"format": map[string]interface{}{"type": "json_schema", "schema": map[string]interface{}{"type": "object"}}},
		Metadata:     map[string]interface{}{"user_id": "user-1"},
	}
	chat, err := anthropicRequestToChat(req)
	testutil.NoError(t, err)
	testutil.Equal(t, chat.Tools[0].Function["strict"], true)
	testutil.Equal(t, len(chat.ResponsesTools), 1)
	testutil.Equal(t, chat.ResponsesTools[0]["type"], "mcp")
	testutil.Equal(t, chat.ResponsesTools[0]["authorization"], "secret")
	testutil.Falsef(t, chat.ResponseText["format"] == nil || chat.SafetyIdentifier != "user-1", "text/safety=%#v %q", chat.ResponseText, chat.SafetyIdentifier)
}

func TestNativeToolsRejectClientAdaptations(t *testing.T) {
	for _, tools := range []interface{}{"invalid", []interface{}{nil}, []interface{}{1, map[string]interface{}{"type": "function", "name": "ok"}}} {
		testutil.Error(t, normalizeBuildResponsesPayload(map[string]interface{}{"tools": tools}))
	}
	for _, kind := range []string{"namespace", "custom", "apply_patch", "local_shell", "tool_search"} {
		payload := map[string]interface{}{"tools": []interface{}{map[string]interface{}{"type": kind, "name": "example", "tools": []interface{}{}}}}
		testutil.Error(t, normalizeBuildResponsesPayload(payload))
	}
	for _, kind := range []string{"agent_message", "local_shell_call", "mcp_tool_call_output", "custom_tool_call", "apply_patch_call_output", "tool_search_call"} {
		payload := map[string]interface{}{"input": []interface{}{map[string]interface{}{"type": kind, "call_id": "call"}}}
		testutil.Error(t, normalizeBuildResponsesPayload(payload))
	}
}

func TestNativeMCPHistoryIsNotLoweredToText(t *testing.T) {
	items := []interface{}{map[string]interface{}{"type": "mcp_call", "name": "lookup", "arguments": `{"count":1.0}`}, map[string]interface{}{"type": "mcp_approval_response", "approval_request_id": "request", "approve": true}}
	payload := map[string]interface{}{"tools": []map[string]interface{}{{"type": "mcp", "server_label": "docs"}}, "input": items}
	before, _ := json.Marshal(items)
	testutil.NoError(t, normalizeBuildResponsesPayload(payload))
	after, _ := json.Marshal(payload["input"])
	testutil.Equal(t, string(after), string(before))
}

func TestNativeToolsPreserveNamesSchemasAndArguments(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "anyOf": []interface{}{map[string]interface{}{"required": []interface{}{"value"}}}, "properties": map[string]interface{}{"value": map[string]interface{}{"type": "integer"}}}
	tool := map[string]interface{}{"type": "function", "name": "Mixed.Case", "parameters": schema}
	item := map[string]interface{}{"type": "function_call", "name": "Mixed.Case", "call_id": "call", "arguments": `{"value":1000.0}`}
	before, _ := json.Marshal(map[string]interface{}{"tool": tool, "item": item})
	payload := map[string]interface{}{"tools": []interface{}{tool}, "input": []interface{}{item}, "tool_choice": map[string]interface{}{"type": "function", "name": "Mixed.Case"}}
	testutil.NoError(t, normalizeBuildResponsesPayload(payload))
	after, _ := json.Marshal(map[string]interface{}{"tool": responses.InterfaceMaps(payload["tools"])[0], "item": responses.InterfaceMaps(payload["input"])[0]})
	testutil.Equal(t, string(after), string(before))
	for _, id := range []string{"grok-4.5-latest", "grok-4.6-latest", "grok-code-fast", "build/grok-4.6", "grok_build/grok-4.6", "grok-4.5-latest-high"} {
		_, ok := ResolveModel(id)
		testutil.False(t, ok, "historical aliases must not resolve")
	}
}
