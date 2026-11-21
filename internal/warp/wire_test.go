package warp

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func testSSE(t *testing.T, event *warpapi.ResponseEvent) string {
	t.Helper()
	raw, err := proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + base64.RawURLEncoding.EncodeToString(raw) + "\n\n"
}
func testDone() *warpapi.ResponseEvent {
	return warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build()}.Build()}.Build()
}
func testOutput(id, text string) *warpapi.Message {
	return warpapi.Message_builder{Id: &id, AgentOutput: warpapi.Message_AgentOutput_builder{Text: &text}.Build()}.Build()
}
func testAction(action *warpapi.ClientAction) *warpapi.ResponseEvent {
	return warpapi.ResponseEvent_builder{ClientActions: warpapi.ResponseEvent_ClientActions_builder{Actions: []*warpapi.ClientAction{action}}.Build()}.Build()
}
func testMessages(messages ...*warpapi.Message) *warpapi.ResponseEvent {
	return testAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{Messages: messages}.Build()}.Build())
}

func TestRequestShape(t *testing.T) {
	raw, err := buildRequest("model-a", preparedRequest{Query: "A real user question 好"})
	if err != nil {
		t.Fatal(err)
	}
	var r warpapi.Request
	if proto.Unmarshal(raw, &r) != nil {
		t.Fatal("protobuf rejected")
	}
	if r.GetSettings().GetModelConfig().GetBase() != "model-a" || r.GetSettings().GetModelConfig().HasCliAgent() || r.GetSettings().GetModelConfig().HasComputerUseAgent() {
		t.Fatal("wrong model config")
	}
	inputs := r.GetInput().GetUserInputs().GetInputs()
	if len(inputs) != 1 || inputs[0].GetUserQuery().GetQuery() != "A real user question 好" {
		t.Fatal("wrong prompt")
	}
	if r.GetMcpContext() != nil || len(r.GetTaskContext().GetTasks()) != 0 || r.GetInput().GetContext().HasCurrentTime() || r.GetMetadata().GetConversationId() != "" {
		t.Fatal("unexpected context")
	}
	s := r.GetSettings()
	fields := s.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		f := fields.Get(i)
		if f.Kind() == protoreflect.BoolKind {
			want := string(f.Name()) == "should_preserve_file_content_in_history"
			if !s.ProtoReflect().Has(f) || s.ProtoReflect().Get(f).Bool() != want {
				t.Fatalf("incorrect history preservation or capability fence: %s", f.Name())
			}
		}
	}
	for _, list := range [][]warpapi.ToolType{s.GetSupportedTools(), s.GetSupportedCliAgentTools()} {
		if len(list) != 1 || list[0] != warpapi.ToolType_SUGGEST_PROMPT {
			t.Fatal("missing passive fence")
		}
	}
}
func TestStreamSnapshotsAppendAndRollback(t *testing.T) {
	stream := testSSE(t, testMessages(testOutput("m", "O"))) + testSSE(t, testAction(warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{Message: testOutput("m", "K")}.Build()}.Build())) + testSSE(t, testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{Message: testOutput("m", "OK")}.Build()}.Build()))
	stream += testSSE(t, testAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build())) + testSSE(t, testMessages(testOutput("n", "discard"))) + testSSE(t, testAction(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build())) + testSSE(t, testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || result.Text != "OK" || !result.Complete {
		t.Fatalf("bad stream result err=%v", err)
	}
}
func TestStreamRejectsUnsafeWithoutCompletion(t *testing.T) {
	messages := []*warpapi.Message{
		warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{}.Build()}.Build(),
		warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{RunShellCommand: warpapi.Message_ToolCall_RunShellCommand_builder{}.Build()}.Build()}.Build(),
		warpapi.Message_builder{WebSearch: warpapi.Message_WebSearch_builder{}.Build()}.Build(),
		warpapi.Message_builder{MessagesReceivedFromAgents: warpapi.Message_MessagesReceivedFromAgents_builder{}.Build()}.Build(),
		warpapi.Message_builder{}.Build(),
	}
	for i, m := range messages {
		stream := testSSE(t, testMessages(testOutput("m", "private partial"))) + testSSE(t, testMessages(m)) + testSSE(t, testDone())
		r, err := readStream(strings.NewReader(stream), "model-a")
		if err == nil || r.Complete {
			t.Fatalf("accepted unsafe case %d", i)
		}
	}
	for _, stream := range []string{"", testSSE(t, testDone()), testSSE(t, testMessages(testOutput("m", "unterminated")))} {
		if _, err := readStream(strings.NewReader(stream), "model-a"); err == nil {
			t.Fatal("accepted empty or incomplete stream")
		}
	}
}
func TestStreamBounds(t *testing.T) {
	text := strings.Repeat("x", 128<<10)
	result, err := readStream(strings.NewReader(testSSE(t, testMessages(testOutput("m", text)))+testSSE(t, testDone())), "model-a")
	if err != nil || result.Text != text {
		t.Fatal("valid answer above the former token-saving output cap was lost")
	}
	_, err = readStream(strings.NewReader(strings.Repeat(": heartbeat\n\n", streamLimit/10)), "model-a")
	if !errors.Is(err, errStreamLimit) {
		t.Fatal("total cap not enforced")
	}
}

func TestStreamAllowsOpaqueServerBookkeepingWithoutText(t *testing.T) {
	for name, message := range map[string]*warpapi.Message{
		"server_tool":  warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{Server: warpapi.Message_ToolCall_Server_builder{Payload: proto.String("bootstrap")}.Build()}.Build()}.Build(),
		"server_event": warpapi.Message_builder{ServerEvent: warpapi.Message_ServerEvent_builder{Payload: proto.String(`{"type":"unknown"}`)}.Build()}.Build(),
	} {
		t.Run(name, func(t *testing.T) {
			// Bookkeeping alone never becomes answer text.
			only := testSSE(t, testMessages(message)) + testSSE(t, testDone())
			if result, err := readStream(strings.NewReader(only), "model-a"); !errors.Is(err, errStreamIncomplete) || result.Text != "" || result.ToolRequested {
				t.Fatalf("bookkeeping produced text or failed as a tool: result=%+v, err=%v", result, err)
			}
			// With a real answer it is inert: no tool flag, no payload echo.
			stream := testSSE(t, testMessages(message)) + testSSE(t, testMessages(testOutput("m", "OK"))) + testSSE(t, testDone())
			result, err := readStream(strings.NewReader(stream), "model-a")
			if err != nil || !result.Complete || result.Text != "OK" || result.ToolRequested || result.ServerBookkeeping != 1 {
				t.Fatalf("bookkeeping not inert: result=%+v, err=%v", result, err)
			}
		})
	}
}

func readStream(source io.Reader, expectedModel string) (streamResult, error) {
	return readStreamWithContext(source, expectedModel, nil)
}

func readStreamWithContext(source io.Reader, expectedModel string, initial []byte) (result streamResult, parseErr error) {
	return readStreamWithProgress(source, expectedModel, initial, nil)
}
