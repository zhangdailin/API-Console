package warp

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"orchids-api/internal/prompt"
)

func TestClientCompatibilityOptions(t *testing.T) {
	const base = `"model":"model-a","messages":[{"role":"user","content":"hi"}]`
	for _, options := range []string{
		`"store":false`,
		`"stream_options":{}`,
		`"stream_options":{"include_usage":true}`,
		`"store":false,"stream_options":{"include_usage":false}`,
	} {
		if err := validateRawRequest(json.RawMessage(`{` + base + `,` + options + `}`)); err != nil {
			t.Fatalf("supported client options rejected: %s: %v", options, err)
		}
	}
	for _, value := range []string{`true`, `null`, `0`, `"false"`, `[]`, `{}`} {
		assertRawRequestRejected(t, `{`+base+`,"store":`+value+`}`)
	}
	for _, value := range []string{`null`, `false`, `0`, `"options"`, `[]`, `{"include_usage":null}`, `{"include_usage":0}`, `{"include_usage":"true"}`, `{"unknown-private-option":false}`} {
		raw := `{` + base + `,"stream_options":` + value + `}`
		assertRawRequestRejected(t, raw)
		if err := validateRawRequest(json.RawMessage(raw)); strings.Contains(err.Error(), "unknown-private-option") {
			t.Fatal("unknown option name reached public errors")
		}
	}
}

func TestClientCompatibilityEagerToolHintPreservesDefinition(t *testing.T) {
	for _, eager := range []bool{false, true} {
		tool := map[string]any{
			"name": "ask_user_question", "description": "Keep this description.",
			"input_schema":          map[string]any{"type": "object", "properties": map[string]any{"question": map[string]any{"type": "string"}}, "required": []any{"question"}},
			"eager_input_streaming": eager,
		}
		before, _ := json.Marshal(tool)
		request := historyRequest(textMessage("user", "Ask a question."))
		request.Tools = []any{tool}
		raw, _ := json.Marshal(map[string]any{"model": "model-a", "messages": request.Messages, "tools": request.Tools})
		if err := validateRawRequest(raw); err != nil {
			t.Fatal(err)
		}
		prepared, err := validateRequest(request)
		if err != nil || len(prepared.Tools) != 1 {
			t.Fatalf("eager tool rejected: %v", err)
		}
		got := prepared.Tools[0]
		if got.Name != tool["name"] || got.Description != tool["description"] || !reflect.DeepEqual(got.Schema, tool["input_schema"]) {
			t.Fatal("eager hint changed the tool definition")
		}
		after, _ := json.Marshal(tool)
		if string(before) != string(after) {
			t.Fatal("normalizing eager hints mutated the caller's tool")
		}
	}
	for _, value := range []any{nil, 0, "true", []any{}, map[string]any{}} {
		tool := map[string]any{"name": "ask_user_question", "input_schema": map[string]any{}, "eager_input_streaming": value}
		if _, err := convertToolsWithError([]any{tool}); err == nil || !strings.Contains(err.Error(), "tools[0].eager_input_streaming") {
			t.Fatalf("malformed eager hint accepted or error lost its path: %v", err)
		}
	}
}

func TestClientCompatibilityDeveloperInstructionsAndUserContext(t *testing.T) {
	context := strings.Repeat("c", 29959)
	request := historyRequest(
		textMessage("developer", "Developer before."),
		textMessage("user", "Hi"),
		textMessage("user", context),
		textMessage("system", "System between."),
		textMessage("user", "Environment context."),
		textMessage("user", "Current question."),
		prompt.Message{Role: "developer", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "text", Text: "Developer after.", CacheControl: &prompt.CacheControl{Type: "ephemeral"}}}}},
	)
	request.System = []prompt.SystemItem{{Type: "text", Text: "Top instructions."}}
	raw, _ := json.Marshal(map[string]any{"model": "model-a", "system": request.System, "messages": request.Messages})
	if err := validateRawRequest(raw); err != nil {
		t.Fatalf("raw developer instructions rejected: %v", err)
	}
	before, _ := json.Marshal(request.Messages)
	prepared, err := validateRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	wantContext := "Hi\n\n" + context + "\n\nEnvironment context.\n\nCurrent question."
	want := "System instructions:\nTop instructions.\nDeveloper before.\nSystem between.\nDeveloper after.\n\nUser: " + wantContext
	if prepared.Query != want || prepared.CurrentText != wantContext {
		t.Fatal("instructions or adjacent user context were reordered, omitted, or promoted")
	}
	after, _ := json.Marshal(request.Messages)
	if string(before) != string(after) {
		t.Fatal("normalizing user context mutated the original messages")
	}
}

func TestClientCompatibilityContextBudgetsAndToolIsolation(t *testing.T) {
	for name, messages := range map[string][]prompt.Message{
		"empty_last_message":     {textMessage("user", "context"), textMessage("user", "")},
		"blank_last_message":     {textMessage("user", "context"), textMessage("user", " \n")},
		"oversized_last_message": {textMessage("user", "context"), textMessage("user", strings.Repeat("u", userTextLimit+1))},
		"oversized_context":      {textMessage("user", strings.Repeat("c", transcriptLimit)), textMessage("user", "now")},
		"assistant_first":        {textMessage("assistant", "invalid history"), textMessage("user", "context"), textMessage("user", "now")},
		"tool_results_not_text": {
			textMessage("user", "context"),
			{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "unbound", Content: "result"}}}},
		},
		"developer_tool_call": {
			{Role: "developer", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "unbound", Name: "echo", Input: map[string]any{}}}}},
			textMessage("user", "now"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := validateRequest(historyRequest(messages...)); err == nil {
				t.Fatal("invalid context or tool ownership accepted")
			}
		})
	}
	// The original final-message and complete current-turn boundaries are exact.
	request := historyRequest(textMessage("user", strings.Repeat("c", transcriptLimit-len("User: ")-len("\n\n")-userTextLimit)), textMessage("user", strings.Repeat("u", userTextLimit)))
	prepared, err := validateRequest(request)
	if err != nil || len(prepared.Query) != transcriptLimit {
		t.Fatalf("exact current context boundary rejected: %v", err)
	}
	request.Messages[0].Content.Text += "c"
	if _, err := validateRequest(request); err == nil {
		t.Fatal("oversized joined context accepted")
	}
}
