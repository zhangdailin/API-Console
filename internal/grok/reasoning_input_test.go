package grok

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeNativeReasoningHistoryDoesNotMutateProofOrCaller(t *testing.T) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(`{"input":[{"type":"reasoning","id":"r1","status":"completed","content":null,"encrypted_content":"Exact+/Proof==","summary":[{"type":"summary_text","text":" Plan\n","annotations":null}],"internal_chat_message_metadata_passthrough":{"secret":"private"}},{"type":"function_call","name":"lookup","call_id":"c1","arguments":"{\"content\":null}"},{"type":"compaction","encrypted_content":"compact-proof"}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	input := payload["input"]
	before, _ := json.Marshal(input)
	if err := normalizeBuildResponsesPayload(payload); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("caller input mutated")
	}
	items := payload["input"].([]any)
	reason := items[0].(map[string]any)
	if reason["encrypted_content"] != "Exact+/Proof==" || reason["id"] != "r1" {
		t.Fatal(reason)
	}
	for _, key := range []string{"content", "status", "internal_chat_message_metadata_passthrough"} {
		if _, exists := reason[key]; exists {
			t.Fatalf("output-only field survived: %s", key)
		}
	}
	if !reflect.DeepEqual(items[1:], input.([]any)[1:]) {
		t.Fatal("tool arguments or compaction changed")
	}
	part := reason["summary"].([]any)[0].(map[string]any)
	if part["text"] != " Plan\n" {
		t.Fatal("summary text rewritten")
	}
}

func TestReasoningRecoveryLabelsHistoricalAnalysis(t *testing.T) {
	payload := map[string]any{"input": []any{
		map[string]any{"type": "reasoning", "encrypted_content": "bad-proof", "summary": []any{map[string]any{"text": "private plan"}}},
		map[string]any{"type": "compaction", "encrypted_content": "client-proof"},
	}}
	if !stripInjectedReasoningReplay(payload) {
		t.Fatal("recovery did not remove bad proof")
	}
	items := payload["input"].([]any)
	text, ok := items[0].(map[string]any)["content"].(string)
	if !ok || text != "Prior model reasoning summary:\nprivate plan" {
		t.Fatalf("unlabelled history: %v", items[0])
	}
	if items[1].(map[string]any)["encrypted_content"] != "client-proof" {
		t.Fatal("compaction proof changed")
	}
	var summaryOnly map[string]any
	_ = json.Unmarshal([]byte(`{"input":[{"type":"reasoning","content":null,"summary":[{"type":"summary_text","text":"private plan"}]}]}`), &summaryOnly)
	normalizeBuildReasoningInput(summaryOnly)
	encoded, _ := json.Marshal(summaryOnly)
	if !strings.Contains(string(encoded), "Prior model reasoning summary:") {
		t.Fatal(string(encoded))
	}
}
