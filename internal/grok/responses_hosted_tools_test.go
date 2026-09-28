package grok

import "testing"

// responsesSearchRequest is what the Grok tools Build sends when the operator
// switches Web search and X search on: both hosted tools travel in the Responses
// `tools` array, beside the ordinary function declarations.
func responsesSearchRequest() ResponsesCreateRequest {
	return ResponsesCreateRequest{
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

func chatRequestDeclaresHostedTool(req *ChatCompletionsRequest, want string) bool {
	for _, tool := range req.Tools {
		if tool.Type == want && nativeToolTypes[want] != "" {
			return true
		}
	}
	for _, tool := range req.ResponsesTools {
		if parseLooseStringAny(tool["type"]) == want {
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
	if err != nil {
		t.Fatalf("chatRequestFromResponses() error = %v", err)
	}
	if !chatRequestDeclaresHostedTool(&chat, "web_search") {
		t.Error("web_search was dropped by the Responses bridge")
	}
	if !chatRequestDeclaresHostedTool(&chat, "x_search") {
		t.Error("x_search was dropped by the Responses bridge")
	}
	if len(chat.Tools) == 0 || chat.Tools[0].Type != "function" {
		t.Fatalf("the function declaration must stay a function tool: %#v", chat.Tools)
	}
}

// The Build plane runs hosted search server-side, so the tool has to appear in
// the payload posted upstream — that is the only place the model can learn that
// browsing was requested.
func TestBuildPayloadForResponsesBridge_AdvertisesHostedSearchTools(t *testing.T) {
	chat, err := chatRequestFromResponses(responsesSearchRequest())
	if err != nil {
		t.Fatalf("chatRequestFromResponses() error = %v", err)
	}
	payload, err := (&Handler{}).responsesPayloadFromChat(ModelSpec{UpstreamModel: "grok-4.20-0309"}, &chat, false)
	if err != nil {
		t.Fatalf("responsesPayloadFromChat() error = %v", err)
	}
	declared := map[string]bool{}
	for _, tool := range interfaceMaps(payload["tools"]) {
		declared[parseLooseStringAny(tool["type"])] = true
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
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		t.Fatal(err)
	}
	declared := map[string]int{}
	for _, tool := range interfaceMaps(payload["tools"]) {
		declared[parseLooseStringAny(tool["type"])]++
	}
	if declared["web_search"] != 1 || declared["x_search"] != 1 {
		t.Fatalf("Build hosted-search route = %#v", payload["tools"])
	}
}

func TestNormalizeBuildResponsesPayloadPreservesExplicitXSearch(t *testing.T) {
	payload := map[string]interface{}{
		"model": "grok-4.7",
		"input": "search X",
		"tools": []interface{}{map[string]interface{}{"type": "x_search"}},
	}
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		t.Fatal(err)
	}
	tools := interfaceMaps(payload["tools"])
	if len(tools) != 1 || parseLooseStringAny(tools[0]["type"]) != "x_search" {
		t.Fatalf("explicit x_search changed: %#v", payload["tools"])
	}
}

// Two identical hosted declarations would reach the upstream as two identical
// searches; the function list is validated for exactly that, and the hosted
// list must not be the way around it.
func TestChatRequestFromResponses_DeduplicatesHostedTools(t *testing.T) {
	req := responsesSearchRequest()
	req.Tools = append(req.Tools, map[string]interface{}{"type": "web_search_preview"})
	chat, err := chatRequestFromResponses(req)
	if err != nil {
		t.Fatalf("chatRequestFromResponses() error = %v", err)
	}
	searches := 0
	for _, tool := range chat.ResponsesTools {
		if parseLooseStringAny(tool["type"]) == "web_search" {
			searches++
		}
	}
	if searches != 1 {
		t.Fatalf("hosted web_search declarations = %d, want 1: %#v", searches, chat.ResponsesTools)
	}
}
