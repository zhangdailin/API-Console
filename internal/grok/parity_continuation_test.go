package grok

import (
	"orchids-api/internal/testutil"

	"testing"
)

func TestAnthropicServerSearchHistoryUsesNativeResponsesItem(t *testing.T) {
	req := anthropicMessagesRequest{
		Model: "grok-4.5", MaxTokens: 64,
		Tools: []anthropicTool{{Type: "web_search_20250305", Name: "web_search"}},
		Messages: []anthropicMessage{{Role: "assistant", Content: []interface{}{
			map[string]interface{}{"type": "server_tool_use", "id": "srv_1", "name": "web_search", "input": map[string]interface{}{"query": "orchids"}},
			map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srv_1", "content": []interface{}{
				map[string]interface{}{"type": "web_search_result", "url": "https://example.com/source"},
			}},
		}}, {Role: "user", Content: "continue"}},
	}
	chat, err := anthropicRequestToChat(req)
	testutil.NoError(t, err)
	payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.5"}, &chat, true)
	testutil.NoError(t, err)
	input := payload["input"].([]interface{})
	call, _ := input[0].(map[string]interface{})
	testutil.Equal(t, call["type"], "web_search_call")
	testutil.Equal(t, call["status"], "completed")
	action := call["action"].(map[string]interface{})
	testutil.Equal(t, action["query"], "orchids")
	testutil.Equal(t, len(action["sources"].([]interface{})), 1)
}
