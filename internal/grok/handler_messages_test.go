package grok

import (
	"bytes"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/testutil"
	"strings"
	"testing"

	"encoding/json"
)

func TestAnthropicRequestToChatPreservesToolsAndMultimodalContent(t *testing.T) {
	req := anthropicMessagesRequest{
		Model:     "grok-4.6",
		MaxTokens: 512,
		System:    []interface{}{map[string]interface{}{"type": "text", "text": "Be precise."}},
		Messages: []anthropicMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "text", "text": "Inspect this"},
				map[string]interface{}{"type": "image", "source": map[string]interface{}{
					"type": "base64", "media_type": "image/png", "data": "YWJj",
				}},
			}},
			{Role: "assistant", Content: []interface{}{
				map[string]interface{}{"type": "tool_use", "id": "tool_1", "name": "Read", "input": map[string]interface{}{"path": "a.txt"}},
			}},
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "tool_result", "tool_use_id": "tool_1", "content": "done"},
				map[string]interface{}{"type": "text", "text": "Now summarize it."},
			}},
		},
		Tools:      []anthropicTool{{Name: "Read", Description: "Read a file", InputSchema: map[string]interface{}{"type": "object"}}},
		ToolChoice: map[string]interface{}{"type": "tool", "name": "Read"},
	}

	chat, err := anthropicRequestToChat(req)
	testutil.NoError(t, err, "conversion error = %v")
	testutil.Falsef(t, chat.MaxTokens == nil || *chat.MaxTokens != 512 || len(chat.Messages) != 5, "unexpected converted request: %#v", chat)
	testutil.Equal(t, chat.Messages[0].Role, "system")
	testutil.Equal(t, chat.Messages[3].Role, "tool")
	testutil.Equal(t, chat.Messages[3].ToolCallID, "tool_1")
	testutil.Equal(t, chat.Messages[4].Role, "user")
	parts, ok := chat.Messages[1].Content.([]interface{})
	if !ok || len(parts) != 2 || !strings.Contains(parts[1].(map[string]interface{})["image_url"].(map[string]interface{})["url"].(string), "data:image/png;base64,YWJj") {
		t.Fatalf("multimodal content mismatch: %#v", chat.Messages[1].Content)
	}
	testutil.Equal(t, len(chat.Messages[2].ToolCalls), 1)
	testutil.Equal(t, len(chat.Tools), 1)
}

func TestAnthropicResponseFromChat(t *testing.T) {
	chat := map[string]interface{}{
		"id": "chat_1",
		"choices": []interface{}{map[string]interface{}{
			"finish_reason": "tool_calls",
			"message": map[string]interface{}{
				"content": "checking",
				"tool_calls": []interface{}{map[string]interface{}{
					"id": "call_1", "function": map[string]interface{}{"name": "Read", "arguments": `{"path":"a.txt"}`},
				}},
			},
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 11, "completion_tokens": 7,
			"prompt_tokens_details": map[string]interface{}{"cached_tokens": 3},
		},
	}
	got := anthropicResponseFromChat("grok-4.6", chat)
	testutil.Equal(t, got["id"], "chat_1")
	testutil.Equal(t, got["stop_reason"], "tool_use")
	content := got["content"].([]interface{})
	testutil.Equal(t, len(content), 2)
	testutil.Equal(t, content[1].(map[string]interface{})["type"], "tool_use")
	usage := got["usage"].(map[string]interface{})
	testutil.Equal(t, usage["input_tokens"], 8)
	testutil.Equal(t, usage["output_tokens"], 7)
	testutil.Equal(t, usage["cache_read_input_tokens"], 3)
}

func TestTranslateOpenAIChatStreamToAnthropic(t *testing.T) {
	input := strings.Join([]string{
		`data: {"id":"chat_1","choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","choices":[{"delta":{"reasoning_content":"think"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chat_1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var out bytes.Buffer
	testutil.NoError(t, translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(input), "grok-4.6", 0), "translate error = %v")
	text := out.String()
	for _, expected := range []string{
		"event: message_start", `"type":"thinking_delta"`, `"text":"hello"`,
		`"type":"tool_use"`, `"type":"input_json_delta"`, `"stop_reason":"tool_use"`, "event: message_stop",
	} {
		testutil.MustContain(t, text, expected)
	}
}

func TestBuildPayloadIncludesMaxOutputTokens(t *testing.T) {
	maxTokens := 321
	payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.6"}, &chatwire.Request{
		Messages: []chatwire.Message{{Role: "user", Content: "hello"}}, MaxTokens: &maxTokens,
	}, false)
	testutil.NoError(t, err)
	testutil.Equal(t, payload["max_output_tokens"], 321)
}

func TestChatRequestUnmarshalPreservesMaxTokens(t *testing.T) {
	var req chatwire.Request
	testutil.NoError(t, json.Unmarshal([]byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"}],"max_tokens":123}`), &req))
	testutil.Falsef(t, req.MaxTokens == nil || *req.MaxTokens != 123, "max_tokens=%v", req.MaxTokens)
}
