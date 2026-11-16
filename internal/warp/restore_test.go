package warp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func historyRequest(messages ...prompt.Message) upstream.UpstreamRequest {
	return upstream.UpstreamRequest{Model: "model-a", Messages: messages}
}

func textMessage(role, text string) prompt.Message {
	return prompt.Message{Role: role, Content: prompt.MessageContent{Text: text}}
}

func TestMultiTurnTranscriptIsBoundedAndOrdered(t *testing.T) {
	messages := []prompt.Message{textMessage("system", "Be brief.")}
	for i := 0; i < historyTurns; i++ {
		messages = append(messages, textMessage("user", "question"), textMessage("assistant", "answer"))
	}
	messages = append(messages, textMessage("user", "final question"))
	prepared, err := validateRequest(historyRequest(messages...))
	if err != nil {
		t.Fatalf("bounded history rejected: %v", err)
	}
	for _, want := range []string{"System instructions:", "User: question", "Assistant: answer", "User: final question"} {
		if !strings.Contains(prepared.Query, want) {
			t.Fatalf("transcript missing %q", want)
		}
	}
	// A turn beyond the cap is rejected rather than silently paid for.
	extra := append(append([]prompt.Message(nil), messages...), textMessage("assistant", "answer"), textMessage("user", "too many"))
	if _, err := validateRequest(historyRequest(extra...)); err == nil {
		t.Fatal("over-long history accepted")
	}
}

func TestTranscriptDropsOldestPartsAtByteCeiling(t *testing.T) {
	big := strings.Repeat("x", 40<<10)
	prepared, err := validateRequest(historyRequest(
		textMessage("user", big), textMessage("assistant", big), textMessage("user", "now"),
	))
	if err != nil {
		t.Fatalf("oversized transcript rejected instead of trimmed: %v", err)
	}
	if len(prepared.Query) > transcriptLimit+64 || !strings.Contains(prepared.Query, "[earlier history omitted]") || !strings.Contains(prepared.Query, "now") {
		t.Fatalf("transcript not bounded: %d bytes", len(prepared.Query))
	}
}

func TestToolCallsAreRenderedAsTextForTheNextTurn(t *testing.T) {
	assistant := prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
		{Type: "text", Text: "checking"},
		{Type: "tool_use", ID: "call-1", Name: "lookup", Input: map[string]any{"q": "x"}},
	}}}
	result := prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
		{Type: "tool_result", ToolUseID: "call-1", Content: "found it"},
	}}}
	prepared, err := validateRequest(historyRequest(textMessage("user", "go"), assistant, result))
	if err != nil {
		t.Fatalf("tool transcript rejected: %v", err)
	}
	for _, want := range []string{"Assistant tool call (id=call-1, name=lookup)", `{"q":"x"}`, "Tool result (id=call-1): found it"} {
		if !strings.Contains(prepared.Query, want) {
			t.Fatalf("transcript missing %q: %s", want, prepared.Query)
		}
	}
}

func TestClientToolsBecomeMCPContextAndSelectOnlyMCPTool(t *testing.T) {
	prepared, err := validateRequest(upstream.UpstreamRequest{Model: "model-a",
		Messages: []prompt.Message{textMessage("user", "hi")},
		Tools: []any{map[string]any{"type": "function", "function": map[string]any{
			"name": "lookup", "description": "find", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
		}}},
	})
	if err != nil || len(prepared.Tools) != 1 || prepared.Tools[0].Name != "lookup" {
		t.Fatalf("client tool not converted: %+v, %v", prepared, err)
	}
	body, err := buildRequest("model-a", prepared)
	if err != nil {
		t.Fatal(err)
	}
	var request warpapi.Request
	if proto.Unmarshal(body, &request) != nil {
		t.Fatal("request rejected")
	}
	tools := request.GetSettings().GetSupportedTools()
	if len(tools) != 1 || tools[0] != warpapi.ToolType_CALL_MCP_TOOL {
		t.Fatalf("supported tools = %v, want only CALL_MCP_TOOL", tools)
	}
	servers := request.GetMcpContext().GetServers()
	if len(servers) != 1 || servers[0].GetId() != "client-request-tools" {
		t.Fatalf("mcp server not declared: %+v", servers)
	}
	declared := servers[0].GetTools()
	if len(declared) != 1 || declared[0].GetName() != "lookup" || declared[0].GetInputSchema() == nil {
		t.Fatalf("mcp tools not declared: %+v", declared)
	}
}

func TestContextWindowIsSentOnlyWhenObserved(t *testing.T) {
	for _, tc := range []struct {
		window int
		want   bool
	}{{0, false}, {200000, true}} {
		body, err := buildRequest("model-a", preparedRequest{Query: "hi", ContextWindow: tc.window})
		if err != nil {
			t.Fatal(err)
		}
		var request warpapi.Request
		if proto.Unmarshal(body, &request) != nil {
			t.Fatal("request rejected")
		}
		got := request.GetSettings().GetModelConfig().HasBaseModelContextWindowLimit()
		if got != tc.want {
			t.Fatalf("window %d present=%t want %t", tc.window, got, tc.want)
		}
	}
}

func mcpToolCallEvent(t *testing.T, id, name string, args map[string]any) *warpapi.ResponseEvent {
	t.Helper()
	schema, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	return testAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
		TaskId: proto.String("root"),
		Messages: []*warpapi.Message{warpapi.Message_builder{Id: proto.String("message-" + id), ToolCall: warpapi.Message_ToolCall_builder{
			ToolCallId: proto.String(id),
			CallMcpTool: warpapi.Message_ToolCall_CallMCPTool_builder{
				Name: proto.String(name), Args: schema,
			}.Build(),
		}.Build()}.Build()},
	}.Build()}.Build())
}

func TestStreamEmitsClientToolCallsAndCompletesWithoutText(t *testing.T) {
	stream := testSSE(t, compatRootTask("root", testOutput("m", "thinking"))) + testSSE(t, mcpToolCallEvent(t, "call-1", "lookup", map[string]any{"q": "x"})) + testSSE(t, testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || len(result.ToolCalls) != 1 {
		t.Fatalf("tool turn rejected: result=%+v err=%v", result, err)
	}
	if call := result.ToolCalls[0]; call.ID != "call-1" || call.Name != "lookup" || call.Args != `{"q":"x"}` {
		t.Fatalf("tool call not mapped: %+v", call)
	}
}

func TestStreamStillRejectsNonClientTools(t *testing.T) {
	shell := testAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
		Messages: []*warpapi.Message{warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{
			RunShellCommand: warpapi.Message_ToolCall_RunShellCommand_builder{}.Build(),
		}.Build()}.Build()},
	}.Build()}.Build())
	stream := testSSE(t, shell) + testSSE(t, testDone())
	if result, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamTool) || result.Complete {
		t.Fatalf("server-side shell tool accepted: result=%+v err=%v", result, err)
	}
}

func TestClientEmitsToolCallEventAndFinishReason(t *testing.T) {
	stream := testSSE(t, initEvent(t, "conv-9")) + testSSE(t, compatRootTask("root")) + testSSE(t, mcpToolCallEvent(t, "call-9", "lookup", map[string]any{"q": "x"})) + testSSE(t, testDone())
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	defer server.Close()
	req := testRequest()
	req.Tools = []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}}
	var events []upstream.SSEMessage
	account := testAccount()
	account.ID = 9
	ctx := bindingAuthContext(t, 1, "unit-key")
	if err := NewFromAccount(account, testConfig(server.URL)).SendRequestWithPayload(ctx, req, func(m upstream.SSEMessage) { events = append(events, m) }, nil); err != nil {
		t.Fatalf("tool turn failed: %v", err)
	}
	var sawCall, sawFinish bool
	for _, event := range events {
		switch event.Type {
		case "model.tool-call":
			sawCall = event.Event["toolCallId"] == "call-9" && event.Event["toolName"] == "lookup" && event.Event["input"] == `{"q":"x"}`
		case "model.finish":
			sawFinish = event.Event["finishReason"] == "tool_use"
		}
	}
	if calls.Load() != 1 || !sawCall || !sawFinish {
		t.Fatalf("tool event or finish reason missing: %+v", events)
	}
}

// A malformed base64 frame must still fail closed even under a tool turn.
func TestToolStreamFrameIntegrity(t *testing.T) {
	stream := "data: !!!\n\n"
	if _, err := readStream(strings.NewReader(stream), "model-a"); err == nil {
		t.Fatal("malformed frame accepted")
	}
}

func TestToolLoopLimitStopsUnboundedAgentConversations(t *testing.T) {
	messages := []prompt.Message{textMessage("user", "go")}
	for i := 0; i < toolRoundLimit; i++ {
		messages = append(messages,
			prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_use", ID: "call", Name: "lookup", Input: map[string]any{}},
			}}},
			prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_result", ToolUseID: "call", Content: "ok"},
			}}},
		)
	}
	// The limit itself is accepted; one further tool round is refused.
	if _, err := validateRequest(historyRequest(messages...)); err != nil {
		t.Fatalf("limit-bound conversation rejected: %v", err)
	}
	messages = append(messages,
		prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call", Name: "lookup", Input: map[string]any{}}}}},
		prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call", Content: "ok"}}}},
	)
	if _, err := validateRequest(historyRequest(messages...)); err == nil {
		t.Fatal("unbounded tool loop accepted")
	}
}

func TestToolResultsTravelAsTypedMCPInputs(t *testing.T) {
	req := upstream.UpstreamRequest{Model: "model-a",
		Messages: []prompt.Message{
			textMessage("user", "look it up"),
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_use", ID: "call-1", Name: "lookup", Input: map[string]any{"q": "x"}},
			}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_result", ToolUseID: "call-1", Content: "found it"},
			}}},
		},
		Tools: []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
	}
	prepared, err := validateRequest(req)
	if err != nil || len(prepared.ToolResults) != 1 {
		t.Fatalf("tool result not prepared: %+v, %v", prepared, err)
	}
	if prepared.ToolResults[0].ID != "call-1" || prepared.ToolResults[0].Name != "lookup" || prepared.ToolResults[0].Content != "found it" {
		t.Fatalf("tool result mapping wrong: %+v", prepared.ToolResults[0])
	}
	body, err := buildRequest("model-a", prepared)
	if err != nil {
		t.Fatal(err)
	}
	var request warpapi.Request
	if proto.Unmarshal(body, &request) != nil {
		t.Fatal("request rejected")
	}
	inputs := request.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 2 {
		t.Fatalf("expected typed result + query, got %d", len(inputs))
	}
	if query := inputs[1].GetUserQuery().GetQuery(); strings.Contains(query, "found it") {
		t.Fatalf("typed result duplicated into the transcript: %q", query)
	}
	result := inputs[0].GetToolCallResult()
	if result.GetToolCallId() != "call-1" {
		t.Fatalf("tool_call_id = %q", result.GetToolCallId())
	}
	texts := result.GetCallMcpTool().GetSuccess().GetResults()
	if len(texts) != 1 || texts[0].GetText().GetText() != "found it" {
		t.Fatalf("typed MCP result missing: %+v", texts)
	}
}
