package warp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
)

func TestLargeWarpContextAndDefinitionsReachWireIntact(t *testing.T) {
	user := strings.Repeat("large-user-界", 12000)
	description := strings.Repeat("description-", 12000)
	schema := map[string]any{"type": "object", "description": strings.Repeat("schema-", 20000)}
	req := historyRequest(textMessage("user", user))
	req.Tools = []any{clientBudgetTool("echo", schema, description, false)}
	prepared, err := validateRequest(req)
	if err != nil || prepared.Query != user {
		t.Fatalf("large current turn rejected or changed: %v", err)
	}
	raw, err := buildRequest(req.Model, prepared)
	if err != nil {
		t.Fatal(err)
	}
	var wire warpapi.Request
	if proto.Unmarshal(raw, &wire) != nil {
		t.Fatal("invalid protobuf")
	}
	inputs := wire.GetInput().GetUserInputs().GetInputs()
	tools := wire.GetMcpContext().GetServers()[0].GetTools()
	if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != user || len(tools) != 1 || tools[0].GetDescription() != description || !reflect.DeepEqual(tools[0].GetInputSchema().AsMap(), schema) {
		t.Fatal("large query, description or schema was truncated on the wire")
	}
	if !wire.GetSettings().GetShouldPreserveFileContentInHistory() {
		t.Fatal("upstream file-content history preservation is disabled")
	}
}

func TestToolResultsFollowedByContextRemainTypedAndOwned(t *testing.T) {
	for _, isError := range []bool{false, true} {
		for _, blocks := range []bool{false, true} {
			req := longHarnessRequest()
			resultText := strings.Repeat("large-tool-result-", 10000)
			var content any = resultText
			if blocks {
				content = []any{map[string]any{"type": "text", "text": resultText}}
			}
			resultBlock := &req.Messages[len(req.Messages)-1].Content.Blocks[0]
			resultBlock.Content, resultBlock.IsError = content, isError
			context := strings.Repeat("additional-context-", 10000)
			req.Messages = append(req.Messages, textMessage("user", context), textMessage("user", "CURRENT_QUESTION"))
			before, _ := json.Marshal(req)
			prepared, err := validateRequest(req)
			if err != nil || len(prepared.ToolResults) != 1 || prepared.ToolResults[0].ID != "call-8" || prepared.ToolResults[0].Content != resultText || prepared.ToolResults[0].IsError != isError || !strings.Contains(prepared.CurrentText, context+"\n\nCURRENT_QUESTION") || !strings.Contains(prepared.Query, "FIRST_OMITTED_USER") {
				t.Fatalf("mixed current user turn lost typed results, text or history: %v", err)
			}
			after, _ := json.Marshal(req)
			if string(before) != string(after) {
				t.Fatal("normalization mutated caller-owned blocks")
			}
			ctx := bindingAuthContext(t, 1, "synthetic-full-context-owner")
			c := NewFromAccount(testAccount(), testConfig("https://private.invalid"))
			c.account.ID = 7
			calls := 0
			c.http = &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				var wire warpapi.Request
				raw, readErr := io.ReadAll(r.Body)
				if readErr != nil || proto.Unmarshal(raw, &wire) != nil {
					t.Fatal("invalid continuation protobuf")
				}
				inputs := wire.GetInput().GetUserInputs().GetInputs()
				if len(inputs) != 2 || inputs[0].GetToolCallResult().GetToolCallId() != "call-8" || inputs[1].GetUserQuery().GetQuery() != prepared.CurrentText || len(wire.GetTaskContext().GetTasks()) != 1 {
					t.Fatal("typed result/current context lost on bound wire")
				}
				result := inputs[0].GetToolCallResult().GetCallMcpTool()
				if isError {
					if result.GetError().GetMessage() != resultText {
						t.Fatal("large tool error result was truncated")
					}
				} else if result.GetSuccess().GetResults()[0].GetText().GetText() != resultText {
					t.Fatal("large tool success result was truncated")
				}
				stream := integrityStream(t, integrityTaskMessages(continuationText("answer", "OK")), testDone())
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream))}, nil
			})}
			// More context must never downgrade this into an unbound generation.
			if err := c.SendRequestWithPayload(ctx, req, nil, nil); err == nil || calls != 0 {
				t.Fatal("unbound mixed tool result reached upstream")
			}
			state := newContinuationAccumulator(nil)
			continuationApply(t, state, continuationCreate(integrityMCP(t, "bound-call", "call-8", "synthetic_echo", map[string]any{}, proto.String(clientMCPServerID))))
			initial, err := state.snapshot()
			if err != nil || !c.bindings.saveScoped(ctx, 7, req.Model, "call-8", callBinding{ConversationID: "full-context-conversation", RootTaskContext: initial, ToolRounds: 100}) {
				t.Fatal("could not seed an owned high-generation binding")
			}
			if err := c.SendRequestWithPayload(ctx, req, nil, nil); err != nil || calls != 1 {
				t.Fatalf("full owned mixed turn did not reach upstream exactly once: %v", err)
			}
			err = c.SendRequestWithPayload(ctx, req, nil, nil)
			var app *apperrors.AppError
			if !errors.As(err, &app) || app.HTTPStatus != 409 || calls != 1 {
				t.Fatal("removing cost limits enabled duplicate tool-result consumption")
			}
		}
	}
}

func TestLargeHistoricalToolArgumentsAreNotDropped(t *testing.T) {
	arguments := strings.Repeat("large-argument-", 12000)
	req := historyRequest(textMessage("user", "go"),
		prompt.Message{Role: "assistant", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_use", ID: "historical", Name: "echo", Input: map[string]any{"text": arguments}}}}},
		prompt.Message{Role: "user", Content: prompt.MessageContent{Blocks: []prompt.ContentBlock{{Type: "tool_result", ToolUseID: "historical", Content: "result"}}}},
		textMessage("assistant", "finished"), textMessage("user", "next"))
	prepared, err := validateRequest(req)
	if err != nil || !strings.Contains(prepared.Query, arguments) || !strings.HasSuffix(prepared.Query, "User: next") {
		t.Fatalf("large historical arguments rejected or lost: %v", err)
	}
}

func TestMixedTypedAndTextualResultsPreserveCurrentContext(t *testing.T) {
	req := longHarnessRequest()
	req.Messages[len(req.Messages)-1].Content.Blocks = append(req.Messages[len(req.Messages)-1].Content.Blocks,
		prompt.ContentBlock{Type: "tool_result", ToolUseID: "external-untyped-result", Content: "EXTERNAL_CONTEXT"})
	req.Messages = append(req.Messages, textMessage("user", "CURRENT_QUESTION"))
	prepared, err := validateRequest(req)
	if err != nil || len(prepared.ToolResults) != 1 || prepared.ToolResults[0].ID != "call-8" || !strings.Contains(prepared.CurrentText, "EXTERNAL_CONTEXT") || !strings.Contains(prepared.CurrentText, "CURRENT_QUESTION") {
		t.Fatalf("binding would lose current context rendered as text: %v", err)
	}
}
