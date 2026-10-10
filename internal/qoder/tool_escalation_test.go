package qoder

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/upstream"
)

func TestNormalizeCommandEscalation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tool, input string
		remove            bool
	}{
		{"production orphan", "exec_command", `{"cmd":"ipconfig /all","justification":"Query network configuration"}`, true},
		{"qualified tool", "functions.exec_command", `{"cmd":"ipconfig","justification":"query","workdir":"D:/Code"}`, true},
		{"mcp command", "mcp__terminal__run_command", `{"command":"pwd","justification":"query","sandbox_permissions":null}`, true},
		{"blank permission", "exec_command", `{"cmd":"pwd","justification":"query","sandbox_permissions":""}`, true},
		{"explicit escalation", "exec_command", `{"cmd":"ipconfig","justification":"Approve network","sandbox_permissions":"require_escalated"}`, false},
		{"explicit default", "exec_command", `{"cmd":"pwd","justification":"query","sandbox_permissions":"use_default"}`, false},
		{"invalid permission is host decision", "exec_command", `{"cmd":"pwd","justification":"query","sandbox_permissions":true}`, false},
		{"no reason", "exec_command", `{"cmd":"pwd"}`, false},
		{"unrelated tool", "read_file", `{"cmd":"pwd","justification":"query"}`, false},
		{"no command", "exec_command", `{"justification":"query"}`, false},
		{"malformed json", "exec_command", `{"cmd":"pwd","justification":`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeCommandEscalation(tc.tool, tc.input)
			if !tc.remove {
				if got != tc.input {
					t.Fatalf("changed input: %s", got)
				}
				return
			}
			var before, after map[string]interface{}
			if err := json.Unmarshal([]byte(tc.input), &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(got), &after); err != nil {
				t.Fatal(err)
			}
			delete(before, "justification")
			want, _ := json.Marshal(before)
			actual, _ := json.Marshal(after)
			if string(want) != string(actual) {
				t.Fatalf("changed other fields: got %s, want %s", actual, want)
			}
		})
	}
}

func TestConsumeStreamNormalizesSplitCommandEscalation(t *testing.T) {
	t.Parallel()
	body := envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-production","function":{"name":"exec_command","arguments":"{\"cmd\":\"ipconfig /all\","}}]}}]}`) +
		envelope(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"justification\":\"Query network configuration\"}"}}]},"finish_reason":"tool_calls"}]}`) +
		"event:finish\ndata: {}\n\n"
	var calls []upstream.SSEMessage
	result, err := consumeStreamObserved(strings.NewReader(body), true, func(event upstream.SSEMessage) {
		if event.Type == "model.tool-call" {
			calls = append(calls, event)
		}
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ToolCallCount != 1 || len(calls) != 1 || result.FinishReason() != "tool_use" {
		t.Fatalf("unexpected tool completion: %+v, %d calls", result, len(calls))
	}
	call := calls[0].Event
	if call["toolCallId"] != "call-production" || call["toolName"] != "exec_command" || call["input"] != `{"cmd":"ipconfig /all"}` {
		t.Fatalf("unexpected emitted call: %+v", call)
	}
}
