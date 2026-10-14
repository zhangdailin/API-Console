package grok

import (
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"testing"

	"encoding/json"

	"orchids-api/internal/testutil"
)

func TestChatRequestFromResponses_ConvertsInputToolsAndReasoning(t *testing.T) {
	effort := map[string]interface{}{"effort": "high"}
	parallel := false
	req := responses.CreateRequest{
		Model:        "grok-4.20-0309",
		Instructions: "用中文回答",
		Input: []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "上海天气"},
				map[string]interface{}{"type": "input_file", "file_url": "https://example.com/a.pdf"},
			}},
			map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": `{"city":"Shanghai"}`},
			map[string]interface{}{"type": "function_call_output", "call_id": "call_1", "output": `{"temp":25}`},
		},
		Reasoning: effort,
		Tools: []map[string]interface{}{{
			"type":        "function",
			"name":        "get_weather",
			"description": "Get weather",
			"parameters":  map[string]interface{}{"type": "object"},
		}},
		ToolChoice:        map[string]interface{}{"type": "function", "name": "get_weather"},
		ParallelToolCalls: &parallel,
	}

	chatReq, err := responses.ChatRequestFromResponses(req)
	testutil.NoError(t, err, "responses.ChatRequestFromResponses() error: %v")
	testutil.Equal(t, len(chatReq.Messages), 4)
	testutil.Equal(t, chatReq.Messages[0].Role, "system")
	testutil.Equal(t, chatReq.Messages[0].Content, "用中文回答")
	testutil.Equal(t, chatReq.Messages[1].Content.([]interface{})[0].(map[string]interface{})["type"], "text")
	testutil.Equal(t, chatReq.Messages[1].Content.([]interface{})[1].(map[string]interface{})["type"], "file")
	testutil.Equal(t, len(chatReq.Messages[2].ToolCalls), 1)
	testutil.Equal(t, chatReq.Messages[3].Role, "tool")
	testutil.Equal(t, chatReq.Messages[3].ToolCallID, "call_1")
	testutil.Falsef(t, chatReq.ReasoningEffort == nil || *chatReq.ReasoningEffort != "high", "reasoning_effort=%v want high", chatReq.ReasoningEffort)
	testutil.Equal(t, len(chatReq.Tools), 1)
	testutil.Equal(t, chatReq.Tools[0].Function["name"], "get_weather")
	choice := chatReq.ToolChoice.(map[string]interface{})
	fn := choice["function"].(map[string]interface{})
	testutil.Equal(t, fn["name"], "get_weather")
}

func TestResponsesCreateRequest_AcceptsCompatibilityFields(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.20-0309",
		"input":"hello",
		"max_output_tokens":"128",
		"previous_response_id":"resp_prev",
		"store":"false",
		"metadata":{"trace":"abc"},
		"truncation":"auto",
		"include":["message.output_text.logprobs"],
		"background":false
	}`)

	var req responses.CreateRequest
	testutil.NoError(t, json.Unmarshal(raw, &req), "json.Unmarshal() error: %v")
	testutil.False(t, req.StreamProvided, "stream should not be marked provided")
	testutil.Falsef(t, req.MaxOutputTokens == nil || *req.MaxOutputTokens != 128, "max_output_tokens=%v want 128", req.MaxOutputTokens)
	testutil.Equal(t, req.PreviousResponseID, "resp_prev")
	testutil.Equal(t, req.Truncation, "auto")
	testutil.Falsef(t, req.Store == nil || *req.Store != false, "store=%v want false", req.Store)
	testutil.Falsef(t, req.Background == nil || *req.Background != false, "background=%v want false", req.Background)
	testutil.Equal(t, len(req.Include), 1)
	testutil.Equal(t, req.Include[0], "message.output_text.logprobs")
}

func TestHandleResponses_AppliesDefaultStreamWhenOmitted(t *testing.T) {
	h := &Handler{}
	var decoded responses.CreateRequest
	testutil.NoError(t, json.Unmarshal([]byte(`{"model":"grok-4.20-0309","input":"hello"}`), &decoded), "json.Unmarshal() error: %v")
	testutil.False(t, decoded.StreamProvided, "stream should be omitted before handler defaulting")
	h.applyDefaultResponsesStream(&decoded)
	testutil.True(t, decoded.Stream, "omitted stream defaults to true")

	var provided responses.CreateRequest
	testutil.NoError(t, json.Unmarshal([]byte(`{"model":"grok-4.20-0309","input":"hello","stream":true}`), &provided), "json.Unmarshal() provided error: %v")
	h.applyDefaultResponsesStream(&provided)
	testutil.False(t, !provided.Stream, "explicit stream=true should be preserved")
}

func TestValidateResponsesCompatibilityFor_RejectsUnsupportedBridgeFields(t *testing.T) {
	background := true
	for _, req := range []responses.CreateRequest{
		{Background: &background},
		{Truncation: "invalid"},
	} {
		err := responses.ValidateCompatibility(req, true)
		testutil.CheckError(t, err)
	}
}

func TestValidateResponsesCompatibilityFor_AcceptsBridgeFields(t *testing.T) {
	store := true
	if err := responses.ValidateCompatibility(responses.CreateRequest{
		Metadata: map[string]interface{}{"trace": "value"}, Truncation: "auto",
		Include: []string{"reasoning.encrypted_content"}, Store: &store, Stream: true,
	}, true); err != nil {
		t.Fatalf("responses.ValidateCompatibility() error = %v", err)
	}
}

func TestChatRequestFromResponses_PreservesMaxOutputTokens(t *testing.T) {
	maxOutputTokens := 128
	chat, err := responses.ChatRequestFromResponses(responses.CreateRequest{
		Model: "grok-4.6", Input: "hello", MaxOutputTokens: &maxOutputTokens,
	})
	testutil.NoError(t, err, "responses.ChatRequestFromResponses() error = %v")
	testutil.Falsef(t, chat.MaxTokens == nil || *chat.MaxTokens != maxOutputTokens, "MaxTokens=%v want %d", chat.MaxTokens, maxOutputTokens)
}

func TestCopyNativeCLIResponseAddsFailedTerminalOnPrematureEOF(t *testing.T) {
	recorder := httptest.NewRecorder()
	input := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n"
	id, _, _ := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), "text/event-stream", "grok-4.6")
	testutil.Equal(t, id, "resp_1")
	body := recorder.Body.String()
	testutil.MustContainAll(t, body, "event: response.failed", "upstream_stream_incomplete", `"model":"grok-4.6"`)
}

func TestCopyNativeCLIResponseKeepsValidTerminal(t *testing.T) {
	recorder := httptest.NewRecorder()
	input := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\ndata: [DONE]\n\n"
	_, _, _ = copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(input), "text/event-stream", "grok-4.6")
	testutil.Equal(t, strings.Count(recorder.Body.String(), "response.failed"), 0)
}

// A Responses input_file must survive conversion into the chat layer, which
// validates the portable file shape. A nested url was rejected downstream, so
// the round trip has to carry the resolved reference as file_data.
//
// A bare file_id is intentionally not covered: the chat layer only accepts a URL
// or data URI, so an opaque asset id is not representable on this path.
func TestResponsesInputFileRoundTripsIntoChatMessages(t *testing.T) {
	for _, part := range []map[string]interface{}{
		{"type": "input_file", "file": map[string]interface{}{"data": "data:application/pdf;base64,QUFB"}},
		{"type": "input_file", "file_url": "https://example.com/a.pdf"},
		{"type": "input_file", "file_data": "data:application/pdf;base64,QUFB"},
		{"type": "input_file", "file": map[string]interface{}{"url": "https://example.com/b.pdf"}},
	} {
		input := []interface{}{map[string]interface{}{
			"type": "message", "role": "user",
			"content": []interface{}{map[string]interface{}{"type": "input_text", "text": "see attached"}, part},
		}}
		messages, err := responses.InputToMessages(input)
		testutil.Falsef(t, err != nil, "%v: %v", part, err)
		err = chatwire.ValidateMessages(messages)
		testutil.CheckNoError(t, err)
		parts, ok := messages[0].Content.([]interface{})
		if !ok || len(parts) != 2 {
			t.Fatalf("%v: converted content=%#v", part, messages[0].Content)
		}
		filePart, _ := parts[1].(map[string]interface{})
		file, _ := filePart["file"].(map[string]interface{})
		data, _ := file["file_data"].(string)
		testutil.Falsef(t, data == "", "%v: file_data missing after conversion: %#v", part, filePart)
	}
}
