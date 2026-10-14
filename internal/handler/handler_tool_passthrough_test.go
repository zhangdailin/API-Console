package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestHandleMessages_CustomMCPToolCall_RemainsAllowed(t *testing.T) {
	t.Parallel()

	client := &fakePayloadClient{
		eventsByOp: [][]upstream.SSEMessage{{
			{
				Type: "model.tool-call",
				Event: map[string]any{
					"toolCallId": "call_workspace_search_1",
					"toolName":   "workspace_search",
					"input":      `{"query":"router","top_k":5}`,
				},
			},
			{
				Type:  "model.finish",
				Event: map[string]any{"finishReason": "tool_use"},
			},
		}},
	}
	h := newTestHandler(client)

	body := []byte(`{
		"model":"claude-opus-4-5",
		"stream":false,
		"conversation_id":"workbuddy_custom_mcp",
		"messages":[{"role":"user","content":"find router handlers"}],
		"tools":[
			{"type":"function","function":{
				"name":"workspace_search",
				"description":"Search the project for matching code symbols",
				"parameters":{
					"type":"object",
					"properties":{
						"query":{"type":"string"},
						"top_k":{"type":"integer"}
					},
					"required":["query"]
				}
			}}
		]
	}`)

	req := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	out := rec.Body.String()
	testutil.MustContain(t, out, `"type":"tool_use"`)
	testutil.MustContain(t, out, `"name":"workspace_search"`)

	calls := client.snapshotCalls()
	testutil.Equal(t, len(calls), 1)
	testutil.Equal(t, len(calls[0].Tools), 1)

	declared, ok := calls[0].Tools[0].(map[string]any)
	testutil.True(t, ok, "declared tool type=%T want map[string]interface{}")
	function, ok := declared["function"].(map[string]any)
	testutil.True(t, ok, "declared tool function=%T want map[string]interface{}")
	got, _ := function["name"].(string)
	testutil.Falsef(t, got != "workspace_search", "declared tool name=%q want workspace_search", got)
}
