package warp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func dialectTool(schema map[string]any, openAI bool) map[string]any {
	if openAI {
		return map[string]any{"type": "function", "function": map[string]any{"name": "Lookup_天气", "description": " precise description \n", "parameters": schema, "strict": false}}
	}
	return map[string]any{"name": "Lookup_天气", "description": " precise description \n", "input_schema": schema}
}

func TestToolDialectsPreserveSchemasNamesAndDescriptions(t *testing.T) {
	schema := map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string", "description": "城市"}}, "required": []any{"city"}, "additionalProperties": false}
	before, _ := json.Marshal(schema)
	for _, openAI := range []bool{false, true} {
		tool := dialectTool(schema, openAI)
		raw, _ := json.Marshal(map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "lookup"}}, "tools": []any{tool}})
		if err := validateRawRequest(raw); err != nil {
			t.Fatalf("dialect openAI=%t rejected raw: %v", openAI, err)
		}
		prepared, err := validateRequest(upstream.UpstreamRequest{Model: "model-a", Messages: []prompt.Message{textMessage("user", "lookup")}, Tools: []any{tool}})
		if err != nil || len(prepared.Tools) != 1 {
			t.Fatalf("dialect openAI=%t rejected: %v", openAI, err)
		}
		got := prepared.Tools[0]
		encoded, _ := json.Marshal(got.Schema)
		if got.Name != "Lookup_天气" || got.Description != " precise description \n" || string(encoded) != string(before) {
			t.Fatalf("tool data changed: %+v schema=%s", got, encoded)
		}
		body, err := buildRequest("model-a", prepared)
		if err != nil {
			t.Fatal(err)
		}
		var wire warpapi.Request
		if err := proto.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		declared := wire.GetMcpContext().GetServers()[0].GetTools()[0]
		wireSchema, _ := json.Marshal(declared.GetInputSchema().AsMap())
		if declared.GetName() != got.Name || declared.GetDescription() != got.Description || string(wireSchema) != string(before) {
			t.Fatalf("wire tool changed: %s", wireSchema)
		}
	}
	after, _ := json.Marshal(schema)
	if string(after) != string(before) {
		t.Fatal("schema mutated")
	}
}

func TestToolDialectsRejectUnknownAndWrongShapes(t *testing.T) {
	for name, tool := range map[string]any{
		"builtin":           map[string]any{"type": "web_search", "name": "search", "input_schema": map[string]any{}},
		"custom":            map[string]any{"type": "custom", "name": "lookup", "input_schema": map[string]any{}},
		"flat_function":     map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{}},
		"wrong_nested_type": map[string]any{"type": "custom", "function": map[string]any{"name": "lookup", "parameters": map[string]any{}}},
		"mixed":             map[string]any{"type": "function", "name": "other", "function": map[string]any{"name": "lookup", "parameters": map[string]any{}}},
		"missing_schema":    map[string]any{"name": "lookup"},
		"null_schema":       map[string]any{"name": "lookup", "input_schema": nil},
		"array_schema":      map[string]any{"name": "lookup", "input_schema": []any{}},
		"bad_description":   map[string]any{"name": "lookup", "description": false, "input_schema": map[string]any{}},
		"unknown_zero":      map[string]any{"name": "lookup", "input_schema": map[string]any{}, "cache_control": nil},
		"strict_true":       map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{}, "strict": true}},
		"strict_null":       map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{}, "strict": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := convertTools([]any{tool}); ok {
				t.Fatal("unsupported normalized tool accepted")
			}
			raw, _ := json.Marshal(map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "tools": []any{tool}})
			if err := validateRawRequest(raw); err == nil {
				t.Fatal("unsupported raw tool accepted")
			}
		})
	}
	for _, names := range [][2]string{{"lookup", "lookup"}, {"Lookup", "LOOKUP"}, {"K", "K"}, {"S", "ſ"}} {
		tools := []any{map[string]any{"name": names[0], "input_schema": map[string]any{}}, map[string]any{"name": names[1], "input_schema": map[string]any{}}}
		if _, ok := convertTools(tools); ok {
			t.Fatalf("casefold duplicate accepted: %q", names)
		}
	}
	for _, builtin := range []map[string]any{{}, {"type": "web_search"}, {"type": "function", "name": "lookup"}} {
		req := historyRequest(textMessage("user", "hi"))
		req.ResponsesTools = []map[string]any{builtin}
		if _, err := validateRequest(req); err == nil {
			t.Fatal("ResponsesTools accepted")
		}
	}
}

func TestToolChoiceDialectsAndParallelDisable(t *testing.T) {
	for _, choice := range []any{"auto", "none", map[string]any{"type": "auto"}, map[string]any{"type": "none", "disable_parallel_tool_use": false}, map[string]any{"type": "auto", "disable_parallel_tool_use": true}} {
		parallel := true
		req := historyRequest(textMessage("user", "hi"))
		req.Tools = []any{dialectTool(map[string]any{"type": "object"}, false)}
		req.ToolChoice, req.ParallelToolCalls = choice, &parallel
		prepared, err := validateRequest(req)
		if err != nil {
			t.Fatalf("choice %v rejected: %v", choice, err)
		}
		mode, disabled, _ := parseToolChoice(choice)
		if prepared.ParallelTools != (mode == "auto" && !disabled) || (len(prepared.Tools) == 0) != (mode == "none") {
			t.Fatalf("choice %v produced tools=%v parallel=%t", choice, prepared.Tools, prepared.ParallelTools)
		}
		raw, _ := json.Marshal(map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "tool_choice": choice})
		if err := validateRawRequest(raw); err != nil {
			t.Fatalf("valid raw choice rejected: %v", err)
		}
	}
	for _, choice := range []any{"AUTO", " auto ", "required", false, map[string]any{}, map[string]any{"type": "tool", "name": "lookup"}, map[string]any{"type": "auto", "name": "lookup"}, map[string]any{"type": "none", "unknown": nil}, map[string]any{"type": "auto", "disable_parallel_tool_use": nil}, map[string]any{"type": "auto", "disable_parallel_tool_use": 0}} {
		if _, _, ok := parseToolChoice(choice); ok {
			t.Fatalf("invalid choice accepted: %v", choice)
		}
		raw, _ := json.Marshal(map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "tool_choice": choice})
		if err := validateRawRequest(raw); err == nil {
			t.Fatalf("invalid raw choice accepted: %v", choice)
		}
	}
}

func TestNativeToolContentBlocksAllowOnlyExactTextShapes(t *testing.T) {
	valid := `{"model":"model-a","tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"lookup","input":{"q":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"found"}],"is_error":false}]}]}`
	if err := validateRawRequest(json.RawMessage(valid)); err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model    string           `json:"model"`
		Messages []prompt.Message `json:"messages"`
		Tools    []any            `json:"tools"`
	}
	if err := json.Unmarshal([]byte(valid), &req); err != nil {
		t.Fatal(err)
	}
	prepared, err := validateRequest(upstream.UpstreamRequest{Model: req.Model, Messages: req.Messages, Tools: req.Tools})
	if err != nil || len(prepared.ToolResults) != 1 || prepared.ToolResults[0].Content != "found" {
		t.Fatalf("native tool result not converted: %+v, %v", prepared, err)
	}
	for _, block := range []string{
		`{"type":"tool_use","id":"c","name":"lookup","input":null}`,
		`{"type":"tool_use","id":"c","name":"lookup","input":[]}`,
		`{"type":"tool_use","id":"c","name":"lookup","input":{},"text":""}`,
		`{"type":"tool_result","tool_use_id":"c","content":"ok","unknown":false}`,
		`{"type":"tool_result","tool_use_id":"c","content":"ok","is_error":null}`,
		`{"type":"tool_result","tool_use_id":"c","content":[{"type":"image","text":"ok"}]}`,
		`{"type":"tool_result","tool_use_id":"c","content":[{"type":"text","text":"ok","source":null}]}`,
		`{"type":"tool_result","tool_use_id":"c","content":{}}`,
		`{"type":"image","source":{}}`, `{"type":"file","file_data":""}`,
	} {
		for _, role := range []string{"user", "assistant", "system", "tool", "developer", "unknown"} {
			assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"`+role+`","content":[`+block+`]}]}`)
		}
	}
	assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":[{"type":"tool_use","id":"c","name":"lookup","input":{}}]}]}`)
	assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"c","content":"ok"}]}]}`)
}

func TestOpenAIToolMessagesRoundTripAndUnknownZeroGuards(t *testing.T) {
	raw := `{"model":"model-a","tools":[{"type":"function","function":{"name":"lookup","description":"find","parameters":{},"strict":false}}],"messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"found"}]}`
	if err := validateRawRequest(json.RawMessage(raw)); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []prompt.Message `json:"messages"`
		Tools    []any            `json:"tools"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	prepared, err := validateRequest(upstream.UpstreamRequest{Model: "model-a", Messages: decoded.Messages, Tools: decoded.Tools})
	if err != nil || len(prepared.ToolResults) != 1 || prepared.ToolResults[0].Content != "found" {
		t.Fatalf("OpenAI tool round trip failed: %+v %v", prepared, err)
	}
	for _, role := range []string{"function", "unknown", "User", " user "} {
		assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"`+role+`","content":"hi"}]}`)
	}
	for _, value := range []string{"null", "false", "0", `""`, "[]", "{}"} {
		assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","unknown":`+value+`}}`)
		assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"ok","unknown":`+value+`}]}]}`)
		assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"system":[{"type":"text","text":"pinned","unknown":`+value+`}]}`)
	}
	for _, args := range []string{`null`, `[]`, `false`, `1`, `"text"`, `{`} {
		arguments, _ := json.Marshal(args)
		assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":`+string(arguments)+`}}]}]}`)
	}
}

func TestTranscriptExactCeilingExcludesSystemButIncludesDialogAndToolResults(t *testing.T) {
	// This transcript contains one history pair and a current user; its byte
	// boundary includes dialog labels/separators but excludes the system prefix.
	messages := []prompt.Message{textMessage("user", "old"), textMessage("assistant", ""), textMessage("user", "now")}
	overhead, ok := renderTranscript("", []prompt.Message{textMessage("user", "old"), textMessage("assistant", "x"), textMessage("user", "now")})
	if !ok {
		t.Fatal("fixture rejected")
	}
	messages[1].Content.Text = strings.Repeat("x", (64<<10)-(len(overhead)-1))
	query, ok := renderTranscript("pinned", messages)
	if !ok || len(query) != (64<<10)+len("System instructions:\npinned\n\n") {
		t.Fatalf("exact boundary rejected or changed: %d %t", len(query), ok)
	}
	messages[1].Content.Text += "x"
	query, ok = renderTranscript("pinned", messages)
	if !ok || len(query) != (64<<10)+1+len("System instructions:\npinned\n\n") || !strings.HasPrefix(query, "System instructions:\npinned\n\n") || strings.Contains(query, "[earlier history omitted]") || !strings.Contains(query, "Assistant:") {
		t.Fatalf("full history or pinned prefix broken: %d %t", len(query), ok)
	}
	assistant := prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "c1", Name: "lookup", Input: map[string]any{}}}}}
	current := prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "c1", Content: ""}}}}
	req := historyRequest(textMessage("system", "pinned"), textMessage("user", "go"), assistant, current)
	req.Tools = []any{map[string]any{"name": "lookup", "input_schema": map[string]any{}}}
	base, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	baseLength := len(appendToolResultsText(base.Query, base.ToolResults)) - len("System instructions:\npinned\n\n")
	req.Messages[3].Content.Blocks[0].Content = strings.Repeat("r", (64<<10)-baseLength)
	boundary, err := validateRequest(req)
	if err != nil || len(appendToolResultsText(boundary.Query, boundary.ToolResults)) != (64<<10)+len("System instructions:\npinned\n\n") {
		t.Fatalf("exact tool-result boundary failed: %v", err)
	}
	req.Messages[3].Content.Blocks[0].Content = strings.Repeat("r", (64<<10)-baseLength+1)
	if _, err := validateRequest(req); err != nil {
		t.Fatal("pinned-system/latest-tool transcript above former cap rejected")
	}
}

func TestNormalizedToolBlocksCannotHideOtherVariants(t *testing.T) {
	for _, block := range []prompt.ContentBlock{
		{Type: "tool_use", ID: "c", Name: "lookup", Input: map[string]any{}, Text: "hidden"},
		{Type: "tool_use", ID: "c", Name: "lookup", Input: []any{}},
		{Type: "tool_result", ToolUseID: "c", Content: map[string]any{"text": "hidden"}},
		{Type: "tool_result", ToolUseID: "c", Content: []any(nil)},
		{Type: "tool_result", ToolUseID: "c", Content: "ok", URL: "https://example.invalid"},
		{Type: "text", Text: "hello", IsError: true},
	} {
		for _, role := range []string{"user", "assistant"} {
			if validMessageBlock(block, role) {
				t.Fatalf("mixed or wrongly shaped normalized block accepted: %+v", block)
			}
		}
	}
}

func TestCurrentUserAndConversationGuardsRemain(t *testing.T) {
	for _, current := range []prompt.Message{textMessage("user", ""), textMessage("user", " \n"), {Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{}}}, {Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: " "}}}}} {
		for _, history := range [][]prompt.Message{nil, {textMessage("system", "pinned"), textMessage("user", "before"), textMessage("assistant", "answer")}} {
			if _, err := validateRequest(historyRequest(append(history, current)...)); err == nil {
				t.Fatal("empty current user accepted")
			}
		}
	}
	messages := []prompt.Message{}
	for i := 0; i < 8; i++ {
		messages = append(messages, textMessage("user", "q"), textMessage("assistant", "a"))
	}
	messages = append(messages, textMessage("user", "current"))
	if _, err := validateRequest(historyRequest(messages...)); err != nil {
		t.Fatalf("8 prior turns rejected: %v", err)
	}
	messages = append(messages, textMessage("assistant", "a"), textMessage("user", "too much"))
	trimmed, err := validateRequest(historyRequest(messages...))
	if err != nil || strings.Contains(trimmed.Query, "[earlier history omitted]") || strings.Count(trimmed.Query, "Assistant: a") != 9 {
		t.Fatal("9 prior turns did not retain full history")
	}
	req := historyRequest(textMessage("user", "hi"))
	req.Model = "auto"
	if _, err := validateRequest(req); err == nil {
		t.Fatal("auto model guard removed")
	}
	original := map[string]any{"name": " lookup ", "input_schema": map[string]any{}}
	tools, ok := convertTools([]any{original})
	if !ok || tools[0].Name != " lookup " || !reflect.DeepEqual(tools[0].Schema, original["input_schema"]) {
		t.Fatal("name bytes or empty schema changed")
	}
}
