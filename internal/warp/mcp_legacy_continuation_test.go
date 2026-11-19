package warp

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func TestMCPLegacyOwnedContinuationKeepsInstallationAndSavesNextCall(t *testing.T) {
	ctx := bindingAuthContext(t, 1, "synthetic-legacy-owner")
	initial := mcpContextSnapshot(t, proto.String(legacyMCPServerID))
	account := testAccount()
	account.ID = 7
	client := NewFromAccount(account, testConfig("https://private.invalid"))
	if !client.bindings.saveScoped(ctx, account.ID, "model-a", "c", callBinding{ConversationID: "old-conversation", RootTaskContext: initial, ToolRounds: 1}) {
		t.Fatal("could not seed an owned pre-upgrade binding")
	}
	request := historyRequest(textMessage("user", "echo"),
		prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "c", Name: "echo", Input: map[string]any{}}}}},
		prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "c", Content: "old result"}}}})
	request.Tools = []any{map[string]any{"name": "echo", "input_schema": map[string]any{"type": "object"}}}
	next := integrityMCP(t, "next-message", "next-call", "echo", map[string]any{}, proto.String(legacyMCPServerID))
	stream := integrityStream(t, initEvent(t, "old-conversation"), integrityTaskMessages(next), testDone())
	calls := 0
	client.http = &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, err := io.ReadAll(r.Body)
		var wire warpapi.Request
		if err != nil || proto.Unmarshal(raw, &wire) != nil {
			t.Fatal("invalid continuation protobuf")
		}
		servers := wire.GetMcpContext().GetServers()
		if len(servers) != 1 || servers[0].GetId() != legacyMCPServerID || wire.GetMetadata().GetConversationId() != "old-conversation" {
			t.Fatal("pre-upgrade continuation switched installations or conversations")
		}
		original := &warpapi.Request_TaskContext{}
		if proto.Unmarshal(initial, original) != nil || !proto.Equal(original, wire.GetTaskContext()) {
			t.Fatal("pre-upgrade continuation history was rewritten")
		}
		inputs := wire.GetInput().GetUserInputs().GetInputs()
		if len(inputs) != 1 || inputs[0].GetToolCallResult().GetToolCallId() != "c" {
			t.Fatal("legacy result was replayed as an unrelated text query")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: r}, nil
	})}
	var events []upstream.SSEMessage
	if err := client.SendRequestWithPayload(ctx, request, func(e upstream.SSEMessage) { events = append(events, e) }, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(events) != 2 || events[0].Type != "model.tool-call" || events[0].Event["toolCallId"] != "next-call" {
		t.Fatal("legacy continuation retried or lost the next callback")
	}
	binding, ok := client.bindings.lookupScoped(ctx, account.ID, "model-a", []string{"next-call"})
	if !ok || binding.ToolRounds != 2 || bytes.Equal(initial, binding.RootTaskContext) {
		t.Fatal("next legacy callback did not retain its updated scoped history")
	}
	root, err := decodeContinuationState(binding.RootTaskContext)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := mcpServerIDForRoot(root); err != nil || id != legacyMCPServerID {
		t.Fatal("next legacy callback cannot resume the same installation")
	}
}
