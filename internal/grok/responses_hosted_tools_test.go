package grok

import (
	"encoding/json"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
)

func TestBuildWebSearchExternalAccessCompatibility(t *testing.T) {
	for _, route := range []string{"responses", "chat"} {
		t.Run(route, func(t *testing.T) {
			search := map[string]any{"type": "web_search", "external_web_access": true,
				"filters": map[string]any{"allowed_domains": []any{"example.com"}}}
			function := map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}
			tools := []map[string]any{search, function}
			before, _ := json.Marshal(tools)
			payload := map[string]any{"model": "grok-4.7", "input": "search", "tools": tools}
			var err error
			if route == "chat" {
				payload, err = (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.7"}, &chatwire.Request{
					Messages: []chatwire.Message{{Role: "user", Content: "search"}}, ResponsesTools: tools,
				}, true)
			} else {
				err = normalizeBuildResponsesPayload(payload)
			}
			testutil.NoError(t, err)
			wire := responses.InterfaceMaps(payload["tools"])
			testutil.Equal(t, len(wire), 3)
			if _, exists := wire[0]["external_web_access"]; exists {
				t.Fatal("Build must not receive external_web_access")
			}
			gotFilters, _ := json.Marshal(wire[0]["filters"])
			wantFilters, _ := json.Marshal(search["filters"])
			testutil.Equal(t, string(gotFilters), string(wantFilters))
			testutil.Equal(t, wire[1]["name"], "lookup")
			testutil.Equal(t, wire[2]["type"], "x_search")
			after, _ := json.Marshal(tools)
			testutil.Equal(t, string(after), string(before))
		})
	}
	for _, value := range []any{false, "true", nil, 1} {
		payload := map[string]any{"tools": []map[string]any{{"type": "web_search", "external_web_access": value}}}
		err := normalizeBuildResponsesPayload(payload)
		if err == nil || !strings.Contains(err.Error(), "tools[0].external_web_access") {
			t.Fatalf("external_web_access=%v: expected explicit local rejection, got %v", value, err)
		}
		if hasBuildHostedTool(responses.InterfaceMaps(payload["tools"]), "x_search") {
			t.Fatal("rejected web search must not enable an X search route")
		}
	}
}

// responsesSearchRequest is what the Grok tools Build sends when the operator
// switches Web search and X search on: both hosted tools travel in the Responses
// `tools` array, beside the ordinary function declarations.
func responsesSearchRequest() responses.CreateRequest {
	return responses.CreateRequest{
		Model: "grok-4.20-0309",
		Input: []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "今天有什么新闻"},
			}},
		},
		Tools: []map[string]any{
			{"type": "web_search"},
			{"type": "x_search"},
			{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}},
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
	chat, err := responses.ChatRequestFromResponses(responsesSearchRequest())
	testutil.NoError(t, err, "responses.ChatRequestFromResponses() error = %v")
	testutil.CheckFalse(t, !chatRequestDeclaresHostedTool(&chat, "web_search"), "web_search was dropped by the Responses bridge")
	testutil.CheckFalse(t, !chatRequestDeclaresHostedTool(&chat, "x_search"), "x_search was dropped by the Responses bridge")
	testutil.Falsef(t, len(chat.Tools) == 0 || chat.Tools[0].Type != "function", "the function declaration must stay a function tool: %#v", chat.Tools)
}

// The Build plane runs hosted search server-side, so the tool has to appear in
// the payload posted upstream — that is the only place the model can learn that
// browsing was requested.
func TestBuildPayloadForResponsesBridge_AdvertisesHostedSearchTools(t *testing.T) {
	chat, err := responses.ChatRequestFromResponses(responsesSearchRequest())
	testutil.NoError(t, err, "responses.ChatRequestFromResponses() error = %v")
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
	payload := map[string]any{
		"model": "grok-4.7",
		"input": "search",
		"tools": []any{map[string]any{"type": "web_search"}},
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
	payload := map[string]any{
		"model": "grok-4.7",
		"input": "search X",
		"tools": []any{map[string]any{"type": "x_search"}},
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
	req.Tools = append(req.Tools, map[string]any{"type": "web_search_preview"})
	chat, err := responses.ChatRequestFromResponses(req)
	testutil.NoError(t, err, "responses.ChatRequestFromResponses() error = %v")
	searches := 0
	for _, tool := range chat.ResponsesTools {
		if chatwire.ParseLooseStringAny(tool["type"]) == "web_search" {
			searches++
		}
	}
	testutil.Equal(t, searches, 1)
}
