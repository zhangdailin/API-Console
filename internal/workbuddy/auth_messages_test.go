package workbuddy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestBuildBodyResponsesParallelToolFollowup(t *testing.T) {
	// Codex sends one Responses item per parallel call, followed by all results.
	// Replay its bridge conversion, not an already grouped Chat history.
	var input any
	testutil.NoError(t, json.Unmarshal([]byte(`[
		{"role":"user","content":"Read three files"},
		{"type":"reasoning","summary":[{"text":"Inspect the files."}]},
		{"role":"assistant","content":"Reading now."},
		{"type":"function_call","call_id":"call-a","name":"read_file","arguments":"{\"path\":\"a\"}"},
		{"type":"function_call","call_id":"call-b","name":"read_file","arguments":"{\"path\":\"b\"}"},
		{"type":"function_call","call_id":"call-c","name":"read_file","arguments":"{\"path\":\"c\"}"},
		{"type":"function_call_output","call_id":"call-a","output":"a result"},
		{"type":"function_call_output","call_id":"call-b","output":"b result"},
		{"type":"function_call_output","call_id":"call-c","output":"c result"}
	]`), &input), "decode input: %v")
	chat, err := responses.InputToMessages(input)
	testutil.NoError(t, err, "Responses conversion: %v")
	raw, err := json.Marshal(chat)
	testutil.NoError(t, err, "encode Chat: %v")
	var history []prompt.Message
	testutil.NoError(t, json.Unmarshal(raw, &history), "decode prompt: %v")
	before, _ := json.Marshal(history)
	body, err := (&Client{}).buildBody(upstream.UpstreamRequest{Model: "deepseek-v4.1-flash", Messages: history})
	testutil.NoError(t, err, "build body: %v")
	var wire struct {
		Messages []ChatMessage `json:"messages"`
	}
	testutil.NoError(t, json.Unmarshal(body, &wire), "decode body: %v")
	testutil.Equal(t, len(wire.Messages), 6)
	assistant := wire.Messages[2]
	testutil.Equal(t, assistant.Role, "assistant")
	testutil.Equal(t, assistant.Content, "Reading now.")
	testutil.Equal(t, assistant.ReasoningContent, "Inspect the files.")
	testutil.Equal(t, len(assistant.ToolCalls), 3)
	for i, id := range []string{"call-a", "call-b", "call-c"} {
		call, result := assistant.ToolCalls[i], wire.Messages[i+3]
		testutil.Equal(t, call.ID, id)
		testutil.Equal(t, call.Function.Name, "read_file")
		testutil.Equal(t, call.Function.Arguments, `{"path":"`+string(rune('a'+i))+`"}`)
		testutil.Equal(t, result.Role, "tool")
		testutil.Equal(t, result.ToolCallID, id)
		testutil.Equal(t, result.Content, string(rune('a'+i))+" result")
	}
	after, _ := json.Marshal(history)
	testutil.Equal(t, string(after), string(before))
}

func TestBuildMessagesKeepsToolTurnAndUserBoundaries(t *testing.T) {
	call := func(id string) prompt.Message {
		return prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: id, Name: "read_file", Input: map[string]any{"path": id}}}}}
	}
	result := func(id string) prompt.Message {
		return prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: id + " result"}}}}
	}
	history := []prompt.Message{userMessage(t, "start"), call("a"), result("a"), call("b"), userMessage(t, "new request"), call("c"), result("b"), result("c"), roleMessage(t, "assistant", "first answer"), roleMessage(t, "assistant", "second answer")}
	messages := buildMessages(upstream.UpstreamRequest{Messages: history})
	want := []string{"system", "user", "assistant", "tool", "assistant", "user", "assistant", "tool", "tool", "assistant", "assistant"}
	var roles []string
	for _, m := range messages {
		roles = append(roles, m.Role)
	}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("turn boundaries changed: got %v want %v", roles, want)
	}
	for i, id := range map[int]string{2: "a", 4: "b", 6: "c"} {
		testutil.Equal(t, len(messages[i].ToolCalls), 1)
		testutil.Equal(t, messages[i].ToolCalls[0].ID, id)
	}
}

func TestBuildMessages_RequiresSystemFirst(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{userMessage(t, "hello")}})
	testutil.Equal(t, len(messages), 2)
	testutil.Equal(t, messages[0].Role, "system")
	testutil.Equal(t, messages[0].Content, defaultSystem)
	testutil.Equal(t, messages[1].Role, "user")
	testutil.Equal(t, messages[1].Content, "hello")
}

func TestBuildMessages_NormalizesDeveloperRole(t *testing.T) {
	t.Parallel()

	// `developer` is OpenAI's alias for the system role; the upstream rejects
	// it, and the rewrite must preserve content and position.
	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{roleMessage(t, "developer", "stay terse")}})
	testutil.Equal(t, len(messages), 1)
	testutil.Equal(t, messages[0].Role, "system")
	testutil.Equal(t, messages[0].Content, "stay terse")
}

func TestBuildMessagesMergesInterleavedSystemWithoutLosingTools(t *testing.T) {
	req := upstream.UpstreamRequest{
		System: []prompt.SystemItem{{Type: "text", Text: "first instruction"}},
		Messages: []prompt.Message{
			roleMessage(t, "developer", "second instruction"),
			userMessage(t, "question one"),
			roleMessage(t, "system", "third instruction"),
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "call-one", Name: "read_file", Input: map[string]any{"path": "file.txt"}}}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "call-one", Content: "file result"}}}},
			userMessage(t, "question two"),
		},
	}
	messages := buildMessages(req)
	if len(messages) != 5 || messages[0].Content != "first instruction\n\nsecond instruction\n\nthird instruction" {
		t.Fatalf("instructions lost: %#v", messages)
	}
	for i, role := range []string{"system", "user", "assistant", "tool", "user"} {
		if messages[i].Role != role {
			t.Fatalf("role order: %#v", messages)
		}
	}
	if messages[2].ToolCalls[0].ID != "call-one" || messages[3].ToolCallID != "call-one" || messages[3].Content != "file result" {
		t.Fatal("tool association lost")
	}
}

func TestBuildMessages_KeepsSystemItemsAndToolResults(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{
		System: []prompt.SystemItem{{Type: "text", Text: "be brief"}},
		Messages: []prompt.Message{{
			Role: "assistant",
			Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "run", Input: map[string]any{}},
			}},
		}, {
			Role: "user",
			Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "text", Text: "run it"},
				{Type: "tool_result", ToolUseID: "toolu_1", Content: "ok"},
			}},
		}},
	})
	testutil.Equal(t, len(messages), 4)
	testutil.Equal(t, messages[0].Role, "system")
	testutil.Equal(t, messages[0].Content, "be brief")
	testutil.Equal(t, messages[3].Role, "tool")
	testutil.Equal(t, messages[3].ToolCallID, "toolu_1")
	testutil.Equal(t, messages[3].Content, "ok")
}

// Claude Code declares itself in the system array. The upstream's policy gate
// answers code=11128 ("blocked by security policy") for such a request, so both
// markers must be dropped before the body is built.
func TestBuildMessages_DropsAnthropicClientMarkers(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{
		System: []prompt.SystemItem{
			{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1.268.e0e; cc_entrypoint=claude-vscode;"},
			{Type: "text", Text: "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."},
			{Type: "text", Text: "be brief"},
		},
		Messages: []prompt.Message{userMessage(t, "hello")},
	})

	testutil.Equal(t, len(messages), 2)
	testutil.Equal(t, messages[0].Role, "system")
	testutil.Equal(t, messages[0].Content, "be brief")
	for _, message := range messages {
		if strings.Contains(message.Content, "anthropic-billing-header") ||
			strings.Contains(message.Content, "official CLI for Claude") {
			t.Fatalf("client marker was forwarded: %+v", message)
		}
	}
}

func TestBuildMessages_ClientMarkerOnlyFallsBackToDefaultSystem(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{
		System:   []prompt.SystemItem{{Type: "text", Text: "You are Claude Code"}},
		Messages: []prompt.Message{userMessage(t, "hello")},
	})

	// Dropping the persona line must not leave the upstream without a leading
	// system message.
	testutil.Equal(t, len(messages), 2)
	testutil.Equal(t, messages[0].Role, "system")
	testutil.Equal(t, messages[0].Content, defaultSystem)
}

func TestBuildMessages_DropsClientMarkerSystemMessage(t *testing.T) {
	t.Parallel()

	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{
		roleMessage(t, "system", "x-anthropic-billing-header: cc_version=2.1.268.e0e; cc_entrypoint=claude-vscode;"),
		userMessage(t, "hello"),
	}})

	for _, message := range messages {
		testutil.MustNotContain(t, message.Content, "anthropic-billing-header")
	}
	testutil.Equal(t, len(messages), 2)
	testutil.Equal(t, messages[0].Content, defaultSystem)
}

func TestBuildMessages_KeepsOrdinaryClaudeMention(t *testing.T) {
	t.Parallel()

	const instruction = "You are an interactive agent. Follow the Claude Code project conventions."

	messages := buildMessages(upstream.UpstreamRequest{
		System:   []prompt.SystemItem{{Type: "text", Text: instruction}},
		Messages: []prompt.Message{userMessage(t, "hello")},
	})
	testutil.Equal(t, messages[0].Content, instruction)
}

func TestBuildMessagesDropsDanglingToolResult(t *testing.T) {
	t.Parallel()
	messages := buildMessages(upstream.UpstreamRequest{Messages: []prompt.Message{{
		Role: "user",
		Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{
			Type: "tool_result", ToolUseID: "missing", Content: "must not be sent",
		}}},
	}}})
	for _, message := range messages {
		testutil.NotEqual(t, message.Role, "tool")
	}
}

func TestBuildMessages_DefaultSystemPromptOnly(t *testing.T) {
	t.Parallel()

	// A request without any history still has to produce a system-first pair,
	// because the upstream rejects anything else with code=11128.
	messages := buildMessages(upstream.UpstreamRequest{})
	testutil.Equal(t, len(messages), 2)
	testutil.Equal(t, messages[0].Role, "system")
	testutil.Equal(t, messages[0].Content, defaultSystem)
	testutil.Equal(t, messages[1].Role, "user")
}

func TestBuildMessagesFiltersClientIdentityAcrossSystemForms(t *testing.T) {
	t.Parallel()
	const instructions = "You are Codex, based on GPT-5.\r\n" +
		"Keep the user's changes.\r\n" +
		"x-anthropic-billing-header: cc_version=test\r\n" +
		"You are Claude Code, Anthropic's official CLI for Claude.\r\n" +
		"Run the relevant tests."
	const want = "Keep the user's changes.\r\nRun the relevant tests."
	for _, form := range []string{"system_items", "system_string", "developer_string", "system_blocks", "developer_blocks"} {
		t.Run(form, func(t *testing.T) {
			req := upstream.UpstreamRequest{Messages: []prompt.Message{userMessage(t, "hello")}}
			switch form {
			case "system_items":
				req.System = []prompt.SystemItem{{Type: "text", Text: instructions}}
			case "system_string", "developer_string":
				role := strings.TrimSuffix(form, "_string")
				req.Messages = append([]prompt.Message{roleMessage(t, role, instructions)}, req.Messages...)
			default:
				role := strings.TrimSuffix(form, "_blocks")
				req.Messages = append([]prompt.Message{{Role: role, Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
					{Type: "text", Text: "  yOu ArE cOdEx.  "},
					{Type: "text", Text: instructions},
				}}}}, req.Messages...)
			}
			messages := buildMessages(req)
			testutil.Equal(t, len(messages), 2)
			testutil.Equal(t, messages[0].Role, "system")
			testutil.Equal(t, messages[0].Content, want)
			testutil.Equal(t, messages[1].Content, "hello")
		})
	}
}

func TestFilterClientSystemTextPreservesRulesAfterCodexIdentity(t *testing.T) {
	t.Parallel()
	for _, identity := range []string{
		"You are Codex, based on GPT-5.1. ",
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. ",
	} {
		const rule = "You are expected to be precise, safe, and helpful. Keep the user's changes."
		testutil.Equal(t, filterClientSystemText(identity+rule), rule)
	}
	const context = "<app-context>\n# Codex desktop context\n- You are running inside the Codex (desktop) app, which allows some additional features not available in the CLI alone:\nUse mcp__codex_app__open_in_codex and codex://review.\nRead C:/Users/example/.codex/skills/SKILL.md.\n</app-context>"
	const want = "<app-context>\n# Desktop context\n- The desktop app provides the following additional features:\nUse mcp__codex_app__open_in_codex and codex://review.\nRead C:/Users/example/.codex/skills/SKILL.md.\n</app-context>"
	testutil.Equal(t, filterClientSystemText(context), want)
	const explanation = "Within this context, Codex refers to the open-source agentic coding interface (not the old Codex language model built by OpenAI).\nFollow project rules."
	testutil.Equal(t, filterClientSystemText(explanation), "Follow project rules.")
}

func TestBuildMessagesCodexMarkerFallbackAndHistoryPreservation(t *testing.T) {
	t.Parallel()
	const marker = "You are Codex."
	messages := buildMessages(upstream.UpstreamRequest{
		System: []prompt.SystemItem{{Type: "text", Text: marker}},
		Messages: []prompt.Message{
			userMessage(t, marker),
			{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "text", Text: marker},
				{Type: "tool_use", ID: "call-codex", Name: "read_file", Input: map[string]any{"path": "README.md"}},
			}}},
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{
				{Type: "tool_result", ToolUseID: "call-codex", Content: marker},
				{Type: "text", Text: marker},
			}}},
		},
	})
	testutil.Equal(t, len(messages), 5)
	testutil.Equal(t, messages[0].Content, defaultSystem)
	for _, message := range messages[1:] {
		testutil.Equal(t, message.Content, marker)
	}
	testutil.Equal(t, messages[2].ToolCalls[0].ID, messages[3].ToolCallID)
	for _, ordinary := range []string{"Use Codex conventions.\r\nKeep this spacing.  ", "You are Codexify, a helper.", "Explain why `You are Codex` appears in requests."} {
		got := buildMessages(upstream.UpstreamRequest{System: []prompt.SystemItem{{Type: "text", Text: ordinary}}})
		testutil.Equal(t, got[0].Content, ordinary)
	}
}
