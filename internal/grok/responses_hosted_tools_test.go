package grok

import (
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"testing"
)

// responsesSearchRequest is what the Grok tools Build sends when the operator
// switches Web search and X search on: both hosted tools travel in the Responses
// `tools` array, beside the ordinary function declarations.
func responsesSearchRequest() responses.CreateRequest {
	return responses.CreateRequest{
		Model: "grok-4.20-0309",
		Input: []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "今天有什么新闻"},
			}},
		},
		Tools: []map[string]interface{}{
			{"type": "web_search"},
			{"type": "x_search"},
			{"type": "function", "name": "lookup", "parameters": map[string]interface{}{"type": "object"}},
		},
	}
}

func chatRequestDeclaresHostedTool(req *chatwire.Request, want string) bool {
	for _, tool := range req.Tools {
		if tool.Type == want && chatwire.NativeToolTypes[want] != "" {
			return true
		}
	}
	for _, tool := range req.ResponsesTools {
		if chatwire.ParseLooseStringAny(tool["type"]) == want {
			return true
		}
	}
	return false
}

// The Responses→Chat bridge used to keep only `function` declarations, so a
// hosted tool the caller had explicitly switched on never reached the upstream:
// the model answered that it had no web access while the Build showed search
// enabled. Both hosted tools must survive the bridge.
func TestChatRequestFromResponses_KeepsHostedSearchTools(t *testing.T) {
	chat, err := chatRequestFromResponses(responsesSearchRequest())
	testutil.NoError(t, err, "chatRequestFromResponses() error = %v")
	testutil.CheckFalse(t, !chatRequestDeclaresHostedTool(&chat, "web_search"), "web_search was dropped by the Responses bridge")
	testutil.CheckFalse(t, !chatRequestDeclaresHostedTool(&chat, "x_search"), "x_search was dropped by the Responses bridge")
	testutil.Falsef(t, len(chat.Tools) == 0 || chat.Tools[0].Type != "function", "the function declaration must stay a function tool: %#v", chat.Tools)
}

// The Build plane runs hosted search server-side, so the tool has to appear in
// the payload posted upstream — that is the only place the model can learn that
// browsing was requested.
func TestBuildPayloadForResponsesBridge_AdvertisesHostedSearchTools(t *testing.T) {
	chat, err := chatRequestFromResponses(responsesSearchRequest())
	testutil.NoError(t, err, "chatRequestFromResponses() error = %v")
	payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.20-0309"}, &chat, false)
	testutil.NoError(t, err, "responsesPayloadFromChat() error = %v")
	declared := map[string]bool{}
	for _, tool := range responses.InterfaceMaps(payload["tools"]) {
		declared[chatwire.ParseLooseStringAny(tool["type"])] = true
	}
	if !declared["web_search"] || !declared["x_search"] {
		t.Fatalf("upstream tools = %#v, want web_search and x_search", payload["tools"])
	}
	if !declared["function"] {
		t.Fatalf("upstream tools = %#v, want the function declaration kept", payload["tools"])
	}
}

func TestNormalizeBuildResponsesPayloadCompletesWebSearchRoute(t *testing.T) {
	payload := map[string]interface{}{
		"model": "grok-4.7",
		"input": "search",
		"tools": []interface{}{map[string]interface{}{"type": "web_search"}},
	}
	testutil.NoError(t, normalizeBuildResponsesPayload(payload))
	declared := map[string]int{}
	for _, tool := range responses.InterfaceMaps(payload["tools"]) {
		declared[chatwire.ParseLooseStringAny(tool["type"])]++
	}
	testutil.Equal(t, declared["web_search"], 1)
	testutil.Equal(t, declared["x_search"], 1)
}

func TestNormalizeBuildResponsesPayloadPreservesExplicitXSearch(t *testing.T) {
	payload := map[string]interface{}{
		"model": "grok-4.7",
		"input": "search X",
		"tools": []interface{}{map[string]interface{}{"type": "x_search"}},
	}
	testutil.NoError(t, normalizeBuildResponsesPayload(payload))
	tools := responses.InterfaceMaps(payload["tools"])
	testutil.Equal(t, len(tools), 1)
	testutil.Equal(t, chatwire.ParseLooseStringAny(tools[0]["type"]), "x_search")
}

// Two identical hosted declarations would reach the upstream as two identical
// searches; the function list is validated for exactly that, and the hosted
// list must not be the way around it.
func TestChatRequestFromResponses_DeduplicatesHostedTools(t *testing.T) {
	req := responsesSearchRequest()
	req.Tools = append(req.Tools, map[string]interface{}{"type": "web_search_preview"})
	chat, err := chatRequestFromResponses(req)
	testutil.NoError(t, err, "chatRequestFromResponses() error = %v")
	searches := 0
	for _, tool := range chat.ResponsesTools {
		if chatwire.ParseLooseStringAny(tool["type"]) == "web_search" {
			searches++
		}
	}
	testutil.Equal(t, searches, 1)
}
