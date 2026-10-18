package qoder

import (
	"encoding/json"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestConversationSessionIdentityIsolation(t *testing.T) {
	first := conversationSessionID("account-a", "conversation-a")
	testutil.Equal(t, first, conversationSessionID("account-a", "conversation-a"))
	testutil.False(t, first == conversationSessionID("account-b", "conversation-a") || first == conversationSessionID("account-a", "conversation-b"), "sessions leaked between accounts or conversations")
}

func TestToolHistoryRetainsIndexAndReasoningWhitespace(t *testing.T) {
	var messages []prompt.Message
	err := json.Unmarshal([]byte(`[{"role":"assistant","content":"","reasoning_content":" thinking\n","reasoning_item":{"type":"reasoning","summary":[]},"tool_calls":[{"id":"call-a","index":0,"type":"function","function":{"name":"Bash","arguments":"{\"command\":\"dir\"}"}}]},{"role":"tool","tool_call_id":"call-a","content":"file.txt"}]`), &messages)
	testutil.NoError(t, err)
	got, _ := buildMessages(upstream.UpstreamRequest{Messages: messages})
	testutil.Falsef(t, len(got) != 2 || got[0].Reasoning != " thinking\n" || len(got[0].ReasoningItem) == 0 || len(got[0].ToolCalls) != 1, "history lost: %+v", got)
	call := got[0].ToolCalls[0]
	testutil.Falsef(t, call.Index == nil || *call.Index != 0 || call.Function.Arguments != `{"command":"dir"}` || got[1].ToolCallID != call.ID || got[1].Content != "file.txt", "tool pairing changed: %+v", got)
	wire, err := json.Marshal(call)
	testutil.NoError(t, err)
	var fields map[string]any
	err = json.Unmarshal(wire, &fields)
	testutil.NoError(t, err)
	_, ok := fields["index"]
	testutil.False(t, !ok, "zero tool index omitted from wire")
}

func TestTextCacheHintSurvivesWireEncoding(t *testing.T) {
	messages := []prompt.Message{{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: "context", CacheControl: &prompt.CacheControl{Type: "ephemeral"}}, {Type: "text", Text: "question"}}}}}
	encoded, err := buildChatBodyProfile(upstream.UpstreamRequest{Messages: messages, Attempt: 2}, modelEntry{Key: "qfmodel"}, "session", "request", "set", DefaultClientVersion, "", sceneBusinessProduct)
	testutil.NoError(t, err)
	raw, err := decodeBody(encoded)
	testutil.NoError(t, err)
	var body chatBody
	err = json.Unmarshal(raw, &body)
	testutil.NoError(t, err)
	testutil.Falsef(t, body.IsRetry || len(body.Messages) != 1 || len(body.Messages[0].Contents) != 2, "unexpected payload: %+v", body)
	parts := body.Messages[0].Contents
	testutil.Falsef(t, parts[0].CacheControl == nil || parts[0].CacheControl.Type != "ephemeral" || parts[1].Text != "question", "cache hint lost: %+v", parts)
}

// Retries of one downstream request keep one task identity (request_set_id and
// business.id) while every attempt gets a fresh request_id; another downstream
// request or another account is another task.
func TestDownstreamRetriesShareTaskSetID(t *testing.T) {
	first := taskSetID("account-a", "request-1")
	testutil.Equal(t, first, taskSetID("account-a", "request-1"))
	testutil.False(t, first == taskSetID("account-b", "request-1") || first == taskSetID("account-a", "request-2"), "task ids leaked between accounts or requests")
	testutil.False(t, first == conversationSessionID("account-a", "request-1"), "task and session ids share a namespace")
}
