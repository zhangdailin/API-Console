package qoder

import (
	"encoding/json"
	"reflect"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestBuildChatBodyMergesResponsesAssistantToolTurn(t *testing.T) {
	t.Parallel()
	var input any
	testutil.NoError(t, json.Unmarshal([]byte(`[
		{"type":"message","role":"user","content":"Find duplicate functions"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"Inspect definitions first."}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will inspect the source."}]},
		{"type":"function_call","call_id":"call-a","name":"exec_command","arguments":"{\"cmd\":\"rg func internal\"}"},
		{"type":"function_call","call_id":"call-b","name":"exec_command","arguments":"{\"cmd\":\"rg func cmd\"}"},
		{"type":"function_call_output","call_id":"call-b","output":"cmd functions"},
		{"type":"function_call_output","call_id":"call-a","output":"internal functions"},
		{"type":"message","role":"assistant","content":"First finding."},
		{"type":"message","role":"assistant","content":"Second finding."},
		{"type":"message","role":"user","content":"Continue"},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"Check the remaining files."}]},
		{"type":"function_call","call_id":"call-c","name":"exec_command","arguments":"{\"cmd\":\"rg func web\"}"},
		{"type":"function_call_output","call_id":"call-c","output":"web functions"}
	]`), &input))
	chat, err := responses.ChatRequestFromResponses(responses.CreateRequest{Model: "test", Input: input})
	testutil.NoError(t, err)
	rawMessages, err := json.Marshal(chat.Messages)
	testutil.NoError(t, err)
	var messages []prompt.Message
	testutil.NoError(t, json.Unmarshal(rawMessages, &messages))
	body := decodeChatBodyForTest(t, upstream.UpstreamRequest{Messages: messages})
	raw, err := json.Marshal(body["messages"])
	testutil.NoError(t, err)
	var got []chatMessage
	testutil.NoError(t, json.Unmarshal(raw, &got))
	testutil.Equal(t, len(got), 9)
	testutil.Equal(t, got[1].Reasoning, "Inspect definitions first.")
	testutil.Equal(t, got[1].Content, "I will inspect the source.")
	testutil.Equal(t, len(got[1].ToolCalls), 2)
	for i, want := range []struct{ id, cmd string }{{"call-a", "rg func internal"}, {"call-b", "rg func cmd"}} {
		testutil.Equal(t, got[1].ToolCalls[i].ID, want.id)
		testutil.Equal(t, got[1].ToolCalls[i].Function.Name, "exec_command")
		testutil.Equal(t, got[1].ToolCalls[i].Function.Arguments, `{"cmd":"`+want.cmd+`"}`)
	}
	testutil.Equal(t, got[2].ToolCallID, "call-b")
	testutil.Equal(t, got[2].Content, "cmd functions")
	testutil.Equal(t, got[3].ToolCallID, "call-a")
	testutil.Equal(t, got[3].Content, "internal functions")
	testutil.Equal(t, got[4].Content, "First finding.")
	testutil.Equal(t, got[5].Content, "Second finding.")
	testutil.Equal(t, got[6].Role, "user")
	testutil.Equal(t, got[7].Reasoning, "Check the remaining files.")
	testutil.Equal(t, got[7].ToolCalls[0].ID, "call-c")
	testutil.Equal(t, got[8].ToolCallID, "call-c")
}

func TestMergeAssistantToolTurnsPreservesReplayAndBoundaries(t *testing.T) {
	t.Parallel()
	call := chatToolCall{ID: "call-a", Type: "function"}
	call.Function.Name, call.Function.Arguments = "exec_command", `{"cmd":"rg func"}`
	for _, boundary := range []chatMessage{
		{Role: "user", Content: "Continue"},
		{Role: "tool", ToolCallID: "earlier", Content: "result"},
		{Role: "system", Content: "New instructions"},
		{Role: "assistant", ReasoningItem: json.RawMessage(`{"id":"other-replay"}`)},
		{Role: "assistant", Meta: json.RawMessage(`{"opaque":true}`)},
		{Role: "assistant", Contents: []chatPart{{Type: "text", Text: "multipart"}}},
	} {
		messages := []chatMessage{
			{Role: "assistant", Reasoning: " first ", ReasoningItem: json.RawMessage(`{"id":"replay"}`)},
			boundary,
			{Role: "assistant", ToolCalls: []chatToolCall{call}},
		}
		before, err := json.Marshal(messages)
		testutil.NoError(t, err)
		got := mergeAssistantToolTurns(messages)
		testutil.Falsef(t, !reflect.DeepEqual(got, messages), "boundary lost: %#v", boundary)
		after, err := json.Marshal(messages)
		testutil.NoError(t, err)
		testutil.Equal(t, string(after), string(before))
	}
	item := json.RawMessage(`{"opaque":"signature"}`)
	got := mergeAssistantToolTurns([]chatMessage{
		{Role: "assistant", Reasoning: " first ", ReasoningItem: item},
		{Role: "assistant", Content: "Inspecting.", Reasoning: "second"},
		{Role: "assistant", ToolCalls: []chatToolCall{call}},
	})
	testutil.Equal(t, len(got), 1)
	testutil.Equal(t, string(got[0].ReasoningItem), string(item))
	testutil.Equal(t, got[0].Reasoning, " first \nsecond")
	testutil.Equal(t, got[0].Content, "Inspecting.")
	testutil.Falsef(t, !reflect.DeepEqual(got[0].ToolCalls, []chatToolCall{call}), "calls changed: %#v", got[0].ToolCalls)
}

func TestAssistantTurnMergePreservesExplicitToolControls(t *testing.T) {
	for _, choice := range []any{"none", "required", map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}}} {
		parallel := false
		body := decodeChatBodyForTest(t, upstream.UpstreamRequest{
			Tools: []any{sampleQoderTool()}, ToolChoice: choice, ParallelToolCalls: &parallel,
			Messages: []prompt.Message{
				{Role: "assistant", Content: prompt.MessageContent{Text: "Read source"}},
				{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call", Name: "read_file", Input: map[string]any{"path": "main.go"}}}}},
			},
		})
		testutil.False(t, !reflect.DeepEqual(body["tool_choice"], choice), "tool choice changed")
		testutil.Equal(t, body["parallel_tool_calls"], false)
		messages := body["messages"].([]any)
		testutil.Equal(t, len(messages), 1)
	}
}
