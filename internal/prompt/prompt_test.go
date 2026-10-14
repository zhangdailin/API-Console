package prompt

import (
	"orchids-api/internal/testutil"
	"testing"

	"encoding/json"
)

func TestMessageUnmarshalJSON_OpenAIToolCallsWithNullContent(t *testing.T) {
	raw := []byte(`{
		"role":"assistant",
		"content":null,
		"tool_calls":[
			{
				"id":"call_write_1",
				"type":"function",
				"function":{
					"name":"Write",
					"arguments":"{\"file_path\":\"note.txt\",\"content\":\"hello world\"}"
				}
			}
		]
	}`)

	var msg Message
	testutil.NoError(t, json.Unmarshal(raw, &msg), "unmarshal message: %v")

	testutil.Equal(t, msg.Role, "assistant")
	testutil.False(t, msg.Content.IsString(), "expected assistant tool call message to normalize into content blocks")

	blocks := msg.Content.GetBlocks()
	testutil.Equal(t, len(blocks), 1)
	testutil.Equal(t, blocks[0].Type, "tool_use")
	testutil.Equal(t, blocks[0].ID, "call_write_1")
	testutil.Equal(t, blocks[0].Name, "Write")
	input, ok := blocks[0].Input.(map[string]any)
	testutil.True(t, ok, "block input type = %T, want map[string]interface{}")
	testutil.Equal(t, input["file_path"], "note.txt")
	testutil.Equal(t, input["content"], "hello world")
}

func TestMessageUnmarshalJSON_OpenAIToolResultMessage(t *testing.T) {
	raw := []byte(`{
		"role":"tool",
		"tool_call_id":"call_write_1",
		"content":"Write succeeded: note.txt created with hello world"
	}`)

	var msg Message
	testutil.NoError(t, json.Unmarshal(raw, &msg), "unmarshal message: %v")

	testutil.Equal(t, msg.Role, "user")
	testutil.False(t, msg.Content.IsString(), "expected tool message to normalize into content blocks")

	blocks := msg.Content.GetBlocks()
	testutil.Equal(t, len(blocks), 1)
	testutil.Equal(t, blocks[0].Type, "tool_result")
	testutil.Equal(t, blocks[0].ToolUseID, "call_write_1")
	got, ok := blocks[0].Content.(string)
	testutil.Falsef(t, !ok || got != "Write succeeded: note.txt created with hello world", "tool_result content = %#v", blocks[0].Content)
}
