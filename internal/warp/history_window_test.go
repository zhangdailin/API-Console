package warp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// Reproduces the 24-message shape without retaining any live request content:
// one developer instruction, fragmented user context, nine history pairs, seven
// tool calls, and a current tool result, with 111 declared client functions.
func longHarnessRequest() upstream.UpstreamRequest {
	req := historyRequest(textMessage("developer", "PINNED_INSTRUCTION"),
		textMessage("user", "FIRST_OMITTED_USER"), textMessage("user", "initial context"), textMessage("user", "more context"))
	for i := 0; i < 9; i++ {
		if i == 5 || i == 6 {
			req.Messages = append(req.Messages, textMessage("assistant", "old answer"), textMessage("user", "next question"), textMessage("user", "context fragment"))
			continue
		}
		id := fmt.Sprintf("call-%d", i)
		req.Messages = append(req.Messages,
			prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: id, Name: "synthetic_echo", Input: map[string]any{"text": "safe"}}}}},
			prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: id, Content: "safe result"}}}},
		)
	}
	for i := 0; i < 111; i++ {
		name := fmt.Sprintf("synthetic_tool_%d", i)
		if i == 0 {
			name = "synthetic_echo"
		}
		req.Tools = append(req.Tools, map[string]any{"name": name, "input_schema": map[string]any{"type": "object"}})
	}
	return req
}

func TestLongHarnessHistoryPreservesAllHistoryAndCurrentToolResult(t *testing.T) {
	req := longHarnessRequest()
	if len(req.Messages) != 24 {
		t.Fatal("fixture no longer matches the observed message count")
	}
	before, _ := json.Marshal(req)
	prepared, err := validateRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Tools) != 111 || len(prepared.ToolResults) != 1 || prepared.ToolResults[0].ID != "call-8" || prepared.ToolResults[0].Name != "synthetic_echo" || prepared.ToolResults[0].Content != "safe result" || prepared.ToolRounds != 0 {
		t.Fatal("history trimming lost tool declarations, current result or trusted budget separation")
	}
	if !strings.Contains(prepared.Query, "FIRST_OMITTED_USER") || strings.Contains(prepared.Query, "[earlier history omitted]") || !strings.HasPrefix(prepared.Query, "System instructions:\nPINNED_INSTRUCTION\n\n") || !strings.Contains(prepared.Query, "id=call-8") {
		t.Fatal("old pairs, pinned instructions or latest tool invocation were not handled correctly")
	}
	if strings.Contains(prepared.CurrentText, "old answer") || !strings.Contains(prepared.CurrentText, "PINNED_INSTRUCTION") {
		t.Fatal("bound continuation replayed old dialog or lost current instructions")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("trimming mutated caller-owned history")
	}
}

func TestHistoryCountAndBytesDoNotTrimConversation(t *testing.T) {
	for _, pairs := range []int{9, 20, 100} {
		messages := []prompt.Message{}
		for i := 0; i < pairs; i++ {
			messages = append(messages, textMessage("user", fmt.Sprintf("old-%03d", i)), textMessage("assistant", strings.Repeat("a", 6000)))
		}
		messages = append(messages, textMessage("user", "CURRENT_MESSAGE"))
		p, err := validateRequest(historyRequest(messages...))
		if err != nil || strings.Count(p.Query, "Assistant:") != pairs || !strings.Contains(p.Query, "old-000") || !strings.HasSuffix(p.Query, "User: CURRENT_MESSAGE") || strings.Contains(p.Query, "[earlier history omitted]") {
			t.Fatalf("history window failed for %d pairs: %v", pairs, err)
		}
	}
	bad := longHarnessRequest()
	bad.Messages[1].Role = "assistant"
	if _, err := validateRequest(bad); err == nil {
		t.Fatal("trimming repaired a malformed old dialog instead of rejecting it")
	}
}

func TestTrimmedCallStillRequiresOwnedBindingBeforeNetwork(t *testing.T) {
	req := longHarnessRequest()
	// Refer to the oldest call, which does not fit the rendered text window.
	// It must still be typed and rejected as unbound, never silently replayed.
	req.Messages[len(req.Messages)-1].Content.Blocks[0].ToolUseID = "call-0"
	prepared, err := validateRequest(req)
	if err != nil || len(prepared.ToolResults) != 1 || prepared.ToolResults[0].ID != "call-0" {
		t.Fatal("trimming downgraded a known tool result to stateless text")
	}
	c := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
	c.account.ID = 7
	called := 0
	c.http = &http.Client{Transport: diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
		called++
		return nil, errors.New("unexpected network")
	})}
	err = c.SendRequestWithPayload(bindingAuthContext(t, 1, "synthetic-window-owner"), req, nil, nil)
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != 409 || called != 0 {
		t.Fatal("trimmed unbound result reached the network or lost its continuation conflict")
	}
}

func TestLongHistoryContinuationHasNoGenerationBudget(t *testing.T) {
	for _, rounds := range []int{1, 6, 7, 100} {
		req := longHarnessRequest()
		ctx := bindingAuthContext(t, 1, "synthetic-window-owner")
		c := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
		c.account.ID = 7
		state := newContinuationAccumulator(nil)
		message := integrityMCP(t, "bound-message", "call-8", "synthetic_echo", map[string]any{}, proto.String(clientMCPServerID))
		continuationApply(t, state, continuationCreate(message))
		initial, err := state.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if !c.bindings.saveScoped(ctx, 7, req.Model, "call-8", callBinding{ConversationID: "window-conversation", RootTaskContext: initial, ToolRounds: rounds}) {
			t.Fatal("binding seed failed")
		}
		calls := 0
		stream := integrityStream(t, integrityTaskMessages(continuationText("answer", "OK")), testDone())
		c.http = &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
			calls++
			raw, readErr := io.ReadAll(r.Body)
			var wire warpapi.Request
			if readErr != nil || proto.Unmarshal(raw, &wire) != nil {
				t.Fatal("invalid resumed protobuf")
			}
			inputs := wire.GetInput().GetUserInputs().GetInputs()
			if len(inputs) != 2 || inputs[0].GetToolCallResult().GetToolCallId() != "call-8" || wire.GetMetadata().GetConversationId() != "window-conversation" || len(wire.GetTaskContext().GetTasks()) != 1 {
				t.Fatal("history window lost the original owned MCP continuation")
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
		})}
		err = c.SendRequestWithPayload(ctx, req, nil, nil)
		if err != nil || calls != 1 {
			t.Fatalf("owned continuation at generation %d was blocked: %v", rounds, err)
		}
	}
}
