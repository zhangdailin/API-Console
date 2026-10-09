package qoder

import (
	"strings"
	"testing"

	"encoding/json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func decodeChatBodyForTest(t *testing.T, req upstream.UpstreamRequest) map[string]interface{} {
	t.Helper()
	encoded, err := buildChatBodyProfile(req, modelEntry{Key: "qmodel_latest", Source: "system"}, "session-id", "request-id", "request-set-id", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err, "buildChatBodyProfile() error = %v")
	raw, err := decodeBodyForTest(encoded)
	testutil.NoError(t, err, "DecodeBody() error = %v")
	var body map[string]interface{}
	testutil.NoError(t, json.Unmarshal(raw, &body), "unmarshal body: %v")
	return body
}

func TestBuildChatBodyDropsDanglingAndDuplicateToolResults(t *testing.T) {
	t.Parallel()
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{Messages: []prompt.Message{
		{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
			Type: "tool_use", ID: "call-1", Name: "read_file", Input: map[string]interface{}{},
		}}}},
		{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
			{Type: "tool_result", ToolUseID: "call-1", Content: "ok"},
			{Type: "tool_result", ToolUseID: "call-1", Content: "duplicate"},
			{Type: "tool_result", ToolUseID: "missing", Content: "dangling"},
		}}},
	}})
	messages, _ := body["messages"].([]interface{})
	toolMessages := 0
	for _, raw := range messages {
		message, _ := raw.(map[string]interface{})
		if message["role"] == "tool" {
			toolMessages++
			testutil.Equal(t, message["tool_call_id"], "call-1")
			testutil.Equal(t, message["content"], "ok")
		}
	}
	testutil.Equal(t, toolMessages, 1)
}

func sampleQoderTool() map[string]interface{} {
	return map[string]interface{}{
		"name":        "read_file",
		"description": "Read a file",
		"input_schema": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"path": map[string]interface{}{"type": "string"}},
		},
	}
}

func TestBuildChatBodyPlacesToolControlsAtTopLevel(t *testing.T) {
	t.Parallel()
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{
		Tools: []interface{}{sampleQoderTool()},
	})
	testutil.Equal(t, body["tool_choice"], "auto")
	parameters, _ := body["parameters"].(map[string]interface{})
	_, exists := parameters["tool_choice"]
	testutil.Falsef(t, exists, "parameters.tool_choice must be absent: %#v", parameters)
	tools, _ := body["tools"].([]interface{})
	testutil.Equal(t, len(tools), 1)
}

func TestBuildChatBodyNormalizesAnthropicToolControls(t *testing.T) {
	t.Parallel()
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{
		Tools: []interface{}{sampleQoderTool()},
		ToolChoice: map[string]interface{}{
			"type":                      "tool",
			"name":                      "read_file",
			"disable_parallel_tool_use": true,
		},
	})
	choice, _ := body["tool_choice"].(map[string]interface{})
	function, _ := choice["function"].(map[string]interface{})
	testutil.Equal(t, choice["type"], "function")
	testutil.Equal(t, function["name"], "read_file")
	parallel, ok := body["parallel_tool_calls"].(bool)
	testutil.Falsef(t, !ok || parallel, "parallel_tool_calls = %#v, want false", body["parallel_tool_calls"])
}

func TestConsumeStreamKeepsTextToolMarkup(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool ca"}}]}`) +
		envelope(`{"choices":[{"delta":{"content":"lls: [{\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"

	var events []upstream.SSEMessage
	result, err := consumeStreamObserved(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		events = append(events, message)
	}, nil)
	testutil.NoError(t, err, "consumeStreamObserved() error = %v")
	testutil.Equal(t, result.FinishReason(), "end_turn")
	testutil.Equal(t, result.ToolCallCount, 0)
	for _, event := range events {
		testutil.Equal(t, event.Type, "model.text-delta")
	}
}

func TestConsumeStreamDoesNotParseTextFallbackWithoutTools(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: []"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	var events []upstream.SSEMessage
	_, err := consumeStreamObserved(strings.NewReader(body), false, func(message upstream.SSEMessage) {
		events = append(events, message)
	}, nil)
	testutil.NoError(t, err, "consumeStreamObserved() error = %v")
	testutil.Equal(t, len(events), 1)
	testutil.Equal(t, events[0].Type, "model.text-delta")
}

func TestConsumeStreamInvalidTextToolFallbackRemainsText(t *testing.T) {
	t.Parallel()
	want := `Tool calls: [{not-json}]`
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: [{not-json}]"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	var text strings.Builder
	result, err := consumeStreamObserved(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			text.WriteString(message.Event["delta"].(string))
		}
	}, nil)
	testutil.NoError(t, err)
	testutil.Falsef(t, text.String() != want || result.ToolCallCount != 0, "text=%q tool calls=%d", text.String(), result.ToolCallCount)
}

func TestConsumeStreamKeepsParallelToolMarkup(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: [{\"id\":\"a\",\"function\":{\"name\":\"read\",\"arguments\":{\"path\":\"a\"}}},{\"id\":\"b\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\":\\\"b\\\"}\"}}]"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	var calls []upstream.SSEMessage
	result, err := consumeStreamObserved(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		if message.Type == "model.tool-call" {
			calls = append(calls, message)
		}
	}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, result.ToolCallCount, 0)
	testutil.Equal(t, len(calls), 0)
}

func TestConsumeStreamNativeToolKeepsSeparateProse(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"content":"Tool calls: "}}]}`) +
		envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"read","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`) +
		"event:finish\ndata: {}\n\n"
	var events []upstream.SSEMessage
	result, err := consumeStreamObserved(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		events = append(events, message)
	}, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, result.ToolCallCount, 1)
	testutil.Equal(t, len(events), 2)
	testutil.Equal(t, events[0].Type, "model.text-delta")
	testutil.Equal(t, events[1].Type, "model.tool-call")
}

func TestConsumeStreamLongSplitWhitespacePrefixStaysLinearAndFlushes(t *testing.T) {
	const chunks = 4096
	var body strings.Builder
	for range chunks {
		body.WriteString(envelope(`{"choices":[{"delta":{"content":" "}}]}`))
	}
	body.WriteString(envelope(`{"choices":[{"delta":{"content":"ordinary text"},"finish_reason":"stop"}]}`))
	body.WriteString("event:finish\ndata: {}\n\n")
	var text strings.Builder
	result, err := consumeStreamObserved(strings.NewReader(body.String()), true, func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			text.WriteString(message.Event["delta"].(string))
		}
	}, nil)
	testutil.NoError(t, err)
	testutil.Falsef(t, text.Len() != chunks+len("ordinary text") || result.ToolCallCount != 0, "text bytes=%d calls=%d", text.Len(), result.ToolCallCount)
}

func TestConsumeStreamOversizedTextFallbackDegradesToText(t *testing.T) {
	large := "Tool calls: " + strings.Repeat("x", 2<<20+1)
	inner, err := json.Marshal(map[string]interface{}{"choices": []interface{}{map[string]interface{}{
		"delta": map[string]interface{}{"content": large}, "finish_reason": "stop",
	}}})
	testutil.NoError(t, err)
	body := envelope(string(inner)) + "event:finish\ndata: {}\n\n"
	textBytes := 0
	result, err := consumeStreamObserved(strings.NewReader(body), true, func(message upstream.SSEMessage) {
		if message.Type == "model.text-delta" {
			textBytes += len(message.Event["delta"].(string))
		}
	}, nil)
	testutil.NoError(t, err)
	testutil.Falsef(t, textBytes != len(large) || result.ToolCallCount != 0, "text bytes=%d want=%d, tool calls=%d", textBytes, len(large), result.ToolCallCount)
}

// TestBuildChatBodyCarriesThinkingSwitch pins the reasoning wire contract: a
// reasoning-capable model row leaves thinking off by default, explicit client
// effort enables it, and "none" turns it off. A non-reasoning model must
// not grow a default thinking flag or effort.
func TestBuildChatBodyCarriesThinkingSwitch(t *testing.T) {
	reasoning := modelEntry{Key: "qwen-plus", Source: "system", IsReasoning: true}
	plain := modelEntry{Key: "qwen-turbo", Source: "system"}

	// Default: even a reasoning-capable model has no reasoning controls.
	body := decodeChatBodyForTestWithModel(t, reasoning, upstream.UpstreamRequest{})
	params, _ := body["parameters"].(map[string]interface{})
	_, present := params["enable_thinking"]
	testutil.Falsef(t, present, "default reasoning model must not enable thinking: %#v", params)
	_, present = params["reasoning_effort"]
	testutil.Falsef(t, present, "default reasoning model must not set effort: %#v", params)
	testutil.Equal(t, body["model_config"].(map[string]interface{})["is_reasoning"], true)
	body = decodeChatBodyForTestWithModel(t, plain, upstream.UpstreamRequest{})
	params, _ = body["parameters"].(map[string]interface{})
	_, present = params["enable_thinking"]
	testutil.Falsef(t, present, "plain model must not carry enable_thinking, got %#v", params)
	_, present = params["reasoning_effort"]
	testutil.Falsef(t, present, "plain model must not carry default reasoning_effort, got %#v", params)

	// An explicit effort turns thinking on, including a mixed-case value.
	body = decodeChatBodyForTestWithModel(t, reasoning, upstream.UpstreamRequest{ReasoningEffort: "HIGH"})
	params, _ = body["parameters"].(map[string]interface{})
	testutil.Falsef(t, params["enable_thinking"] != true || params["reasoning_effort"] != "high" || body["model_config"].(map[string]interface{})["is_reasoning"] != true, "parameters = %#v, want explicit thinking on with effort=high", params)

	// qfmodel is catalogued as reasoning-capable, so model_config reports that
	// capability; the thinking parameters themselves stay off until the client
	// asks for them.
	body = decodeChatBodyForTestWithModel(t, modelEntry{Key: "qfmodel", IsReasoning: true}, upstream.UpstreamRequest{})
	params, _ = body["parameters"].(map[string]interface{})
	testutil.Equal(t, body["model_config"].(map[string]interface{})["is_reasoning"], true)
	_, present = params["reasoning_effort"]
	testutil.Falsef(t, present, "qfmodel default carried reasoning_effort: %#v", params)

	// "none" disables thinking explicitly.
	body = decodeChatBodyForTestWithModel(t, reasoning, upstream.UpstreamRequest{ReasoningEffort: "none"})
	params, _ = body["parameters"].(map[string]interface{})
	testutil.Equal(t, params["enable_thinking"], false)
	_, present = params["reasoning_effort"]
	testutil.Falsef(t, present, "none must not forward an effort level, got %#v", params["reasoning_effort"])
}

func decodeChatBodyForTestWithModel(t *testing.T, model modelEntry, req upstream.UpstreamRequest) map[string]interface{} {
	t.Helper()
	encoded, err := buildChatBodyProfile(req, model, "session-id", "request-id", "request-set-id", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err, "buildChatBodyProfile() error = %v")
	raw, err := decodeBodyForTest(encoded)
	testutil.NoError(t, err, "DecodeBody() error = %v")
	var body map[string]interface{}
	testutil.NoError(t, json.Unmarshal(raw, &body), "unmarshal body: %v")
	return body
}
