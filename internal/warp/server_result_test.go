package warp

import (
	"errors"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func officialServerCall(id, callID, payload string) *warpapi.Message {
	return warpapi.Message_builder{Id: proto.String(id), TaskId: proto.String("root"), ToolCall: warpapi.Message_ToolCall_builder{
		ToolCallId: proto.String(callID), Server: warpapi.Message_ToolCall_Server_builder{Payload: proto.String(payload)}.Build(),
	}.Build()}.Build()
}

func officialServerResult(id, callID, payload string) *warpapi.Message {
	return warpapi.Message_builder{Id: proto.String(id), TaskId: proto.String("root"), ToolCallResult: warpapi.Message_ToolCallResult_builder{
		ToolCallId: proto.String(callID), Server: warpapi.Message_ToolCallResult_ServerResult_builder{SerializedResult: proto.String(payload)}.Build(),
	}.Build()}.Build()
}

func serverResultUpdate(message *warpapi.Message, paths ...string) *warpapi.ResponseEvent {
	return testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{
		TaskId: proto.String("root"), Message: message, Mask: &fieldmaskpb.FieldMask{Paths: paths},
	}.Build()}.Build())
}

func TestServerResultOpaqueRoundtripWithLaterMCP(t *testing.T) {
	payload := "{not JSON; command='never execute'; opaque\x00data}"
	call := officialServerCall("bootstrap", "server-call", "uninterpreted bootstrap")
	resultMessage := officialServerResult("server-result", "server-call", payload)
	stream := integrityStream(t, compatRootTask("root", call), integrityTaskMessages(resultMessage),
		integrityTaskMessages(integrityMCP(t, "client-call", "mcp-call", "lookup", nil, nil)), testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || result.ToolRequested || result.Text != "" || result.TextBytes != 0 || len(result.ToolCalls) != 1 || result.ServerBookkeeping != 2 || result.Rejection != nil {
		t.Fatalf("opaque roundtrip failed: err=%v", err)
	}
	root, err := decodeContinuationState(result.RootTaskContext)
	if err != nil || len(root.GetMessages()) != 3 || !proto.Equal(root.GetMessages()[0], call) || !proto.Equal(root.GetMessages()[1], resultMessage) {
		t.Fatal("official opaque messages not retained exactly")
	}
	prepared := preparedRequest{Query: "continue", RootTaskContext: result.RootTaskContext}
	requestBytes, err := buildRequest("model-a", prepared)
	if err != nil {
		t.Fatal(err)
	}
	request := &warpapi.Request{}
	if proto.Unmarshal(requestBytes, request) != nil || !proto.Equal(request.GetTaskContext().GetTasks()[0], root) {
		t.Fatal("continuation did not roundtrip original official task state")
	}
	// A later response can contain only the result when the initial root has
	// the corresponding Server call. The result itself remains non-output.
	stream = integrityStream(t, serverResultUpdate(resultMessage), integrityTaskMessages(continuationText("answer", "OK")), testDone())
	resumed, err := readStreamWithContext(strings.NewReader(stream), "model-a", result.RootTaskContext)
	if err != nil || resumed.Text != "OK" || resumed.ToolRequested || resumed.ServerBookkeeping != 1 {
		t.Fatalf("initial-root pairing rejected: %v", err)
	}
}

func TestServerResultRejectsUnpairedAndWrongRoot(t *testing.T) {
	for name, messages := range map[string][]*warpapi.Message{
		"alone":        {officialServerResult("result", "call", "opaque")},
		"empty ID":     {officialServerCall("call", "", ""), officialServerResult("result", "", "opaque")},
		"different ID": {officialServerCall("call", "one", ""), officialServerResult("result", "two", "opaque")},
		"future call":  {officialServerResult("result", "call", "opaque"), officialServerCall("call", "call", "")},
	} {
		t.Run(name, func(t *testing.T) {
			stream := integrityStream(t, compatRootTask("root"), integrityTaskMessages(messages...), integrityTaskMessages(continuationText("answer", "OK")), testDone())
			result, err := readStream(strings.NewReader(stream), "model-a")
			if !errors.Is(err, errStreamTool) || result.Complete || result.Rejection == nil || result.Rejection.Reason != "server_unpaired" {
				t.Fatalf("unpaired result accepted or missing diagnostic: %v", err)
			}
		})
	}
	wrong := officialServerResult("result", "call", "opaque")
	wrong.SetTaskId("other-private-id")
	result, err := readStream(strings.NewReader(integrityStream(t, compatRootTask("root", officialServerCall("call", "call", "")), integrityTaskMessages(wrong), testDone())), "model-a")
	if !errors.Is(err, errStreamTool) || result.Rejection == nil || result.Rejection.Reason != "task_mismatch" || result.Rejection.Site != "message_task" || result.Rejection.MessageTaskMatchesRoot || !result.Rejection.HasMessageTaskID || result.Rejection.EventOrdinal != 2 || result.Rejection.ActionOrdinal != 1 || result.Rejection.MessageOrdinal != 1 {
		t.Fatal("wrong-root result lacked content-free mismatch diagnostic")
	}
}

func TestServerResultCannotRepairInvalidTaskState(t *testing.T) {
	call := officialServerCall("call", "call", "")
	badCall := proto.Clone(call).(*warpapi.Message)
	badCall.SetId("")
	for name, events := range map[string][]*warpapi.ResponseEvent{
		"missing root":      {integrityTaskMessages(call)},
		"invalid bootstrap": {compatRootTask("root", badCall)},
		"duplicate message": {compatRootTask("root", call), integrityTaskMessages(proto.Clone(call).(*warpapi.Message))},
	} {
		t.Run(name, func(t *testing.T) {
			events = append(events, integrityTaskMessages(officialServerResult("result", "call", "opaque")), integrityTaskMessages(continuationText("answer", "OK")), testDone())
			result, err := readStream(strings.NewReader(integrityStream(t, events...)), "model-a")
			if err == nil || result.Complete || result.Text != "" {
				t.Fatal("ServerResult permitted invalid original task state")
			}
		})
	}
}

func TestServerResultPayloadByteLimit(t *testing.T) {
	for _, size := range []int{0, serverBookkeepingPayloadLimit, serverBookkeepingPayloadLimit + 1} {
		stream := integrityStream(t, compatRootTask("root", officialServerCall("call", "call", "")),
			integrityTaskMessages(officialServerResult("result", "call", strings.Repeat("x", size))), integrityTaskMessages(continuationText("answer", "OK")), testDone())
		result, err := readStream(strings.NewReader(stream), "model-a")
		if size <= serverBookkeepingPayloadLimit {
			if err != nil || result.Text != "OK" || result.ToolRequested {
				t.Fatalf("bounded result rejected: size=%d err=%v", size, err)
			}
		} else if !errors.Is(err, errStreamLimit) || result.Rejection == nil || result.Rejection.Reason != "server_oversize" || result.Rejection.PayloadBytes != size || result.Rejection.Correlation != "paired_server" {
			t.Fatal("oversized result accepted or missing size diagnostic")
		}
	}
}

func TestServerResultAllNativeAndUnboundMCPResultsRejected(t *testing.T) {
	oneof := (&warpapi.Message_ToolCallResult{}).ProtoReflect().Descriptor().Oneofs().ByName("result")
	for i := 0; i < oneof.Fields().Len(); i++ {
		field := oneof.Fields().Get(i)
		if field.Name() == "server" {
			continue
		}
		t.Run(string(field.Name()), func(t *testing.T) {
			value := warpapi.Message_ToolCallResult_builder{ToolCallId: proto.String("call")}.Build()
			value.ProtoReflect().Set(field, value.ProtoReflect().NewField(field))
			message := warpapi.Message_builder{Id: proto.String("result"), TaskId: proto.String("root"), ToolCallResult: value}.Build()
			stream := integrityStream(t, compatRootTask("root", officialServerCall("call", "call", "")), integrityTaskMessages(message), testDone())
			result, err := readStream(strings.NewReader(stream), "model-a")
			if !errors.Is(err, errStreamTool) || result.Complete || result.Rejection == nil || result.Rejection.ResultKind != int(field.Number()) || !result.Rejection.HasCallID || result.Rejection.HasUnknown {
				t.Fatalf("unsupported result accepted or diagnostic invalid: %v", err)
			}
			want := "unsupported_result"
			if field.Name() == "call_mcp_tool" {
				want = "unbound_mcp"
			}
			if result.Rejection.Reason != want {
				t.Fatalf("reason = %s, want %s", result.Rejection.Reason, want)
			}
		})
	}
}

func TestServerResultRejectsUnknownSchemaAtEveryLevel(t *testing.T) {
	for _, level := range []string{"message", "result", "server"} {
		t.Run(level, func(t *testing.T) {
			message := officialServerResult("result", "call", "opaque")
			target := message.ProtoReflect()
			if level == "result" {
				target = message.GetToolCallResult().ProtoReflect()
			} else if level == "server" {
				target = message.GetToolCallResult().GetServer().ProtoReflect()
			}
			target.SetUnknown([]byte{0xc0, 0x3e, 0x01})
			stream := integrityStream(t, compatRootTask("root", officialServerCall("call", "call", "")), integrityTaskMessages(message), testDone())
			result, err := readStream(strings.NewReader(stream), "model-a")
			if !errors.Is(err, errStreamStream) || result.Complete {
				t.Fatal("unknown ServerResult schema accepted")
			}
		})
	}
}

func TestServerResultTransactionsRestorePairingAndOriginalState(t *testing.T) {
	begin := testAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build())
	rollback := testAction(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build())
	call := officialServerCall("call", "kept", "original call")
	kept := officialServerResult("result", "kept", "original result")
	base := []*warpapi.ResponseEvent{compatRootTask("root", call, kept), begin,
		serverResultUpdate(officialServerResult("result", "kept", "discard result")),
		integrityTaskMessages(officialServerCall("discard-call", "discarded", "discard call")), rollback}
	stream := integrityStream(t, append(base, integrityTaskMessages(integrityMCP(t, "client", "mcp", "lookup", nil, nil)), testDone())...)
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || result.ServerBookkeeping != 2 {
		t.Fatalf("rollback rejected or bookkeeping not restored: %v", err)
	}
	root, err := decodeContinuationState(result.RootTaskContext)
	if err != nil || len(root.GetMessages()) != 3 || !proto.Equal(root.GetMessages()[0], call) || !proto.Equal(root.GetMessages()[1], kept) {
		t.Fatal("rollback failed to restore opaque task state")
	}
	stream = integrityStream(t, append(base, integrityTaskMessages(officialServerResult("bad", "discarded", "unpaired")), testDone())...)
	result, err = readStream(strings.NewReader(stream), "model-a")
	if !errors.Is(err, errStreamTool) || result.Rejection == nil || result.Rejection.Reason != "server_unpaired" {
		t.Fatal("rolled-back Server call remained paired")
	}
	// Creating a task inside a rolled-back transaction restores root identity.
	stream = integrityStream(t, begin, compatRootTask("discard-root"), rollback, compatRootTask("root", call, kept), integrityTaskMessages(continuationText("answer", "OK")), testDone())
	if result, err := readStream(strings.NewReader(stream), "model-a"); err != nil || result.Text != "OK" {
		t.Fatalf("rollback leaked created root identity: %v", err)
	}
}

func TestServerResultCommittedPairingAndMaskedServerCall(t *testing.T) {
	call := officialServerCall("call", "paired", "before")
	delta := officialServerCall("call", "", "after")
	stream := integrityStream(t, compatRootTask("root"),
		testAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build()),
		integrityTaskMessages(call), serverResultUpdate(delta, "tool_call.server.payload"),
		testAction(warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build()),
		integrityTaskMessages(officialServerResult("result", "paired", "opaque")),
		integrityTaskMessages(integrityMCP(t, "client", "mcp", "lookup", nil, nil)), testDone())
	parsed, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || parsed.ToolRequested || len(parsed.ToolCalls) != 1 {
		t.Fatalf("committed masked Server call not paired: %v", err)
	}
	root, err := decodeContinuationState(parsed.RootTaskContext)
	if err != nil || root.GetMessages()[0].GetToolCall().GetToolCallId() != "paired" || root.GetMessages()[0].GetToolCall().GetServer().GetPayload() != "after" {
		t.Fatal("masked Server call lost original identity or payload update")
	}
}

func TestServerResultNativeVariantCannotHideBehindMask(t *testing.T) {
	value := warpapi.Message_ToolCallResult_builder{ToolCallId: proto.String("call"), RunShellCommand: warpapi.RunShellCommandResult_builder{}.Build()}.Build()
	message := warpapi.Message_builder{Id: proto.String("result"), TaskId: proto.String("root"), ToolCallResult: value, ServerMessageData: proto.String("ignored")}.Build()
	stream := integrityStream(t, compatRootTask("root", officialServerCall("call", "call", ""), officialServerResult("result", "call", "safe")),
		serverResultUpdate(message, "server_message_data"), integrityTaskMessages(continuationText("answer", "OK")), testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if !errors.Is(err, errStreamTool) || result.Rejection == nil || result.Rejection.Reason != "unsupported_result" {
		t.Fatal("native result was hidden behind a bookkeeping mask")
	}
}

func TestServerResultMaskedUpdatesValidateEffectiveMessage(t *testing.T) {
	call := officialServerCall("call", "call", "bootstrap")
	result := officialServerResult("result", "call", "before")
	delta := officialServerResult("result", "", "after")
	stream := integrityStream(t, compatRootTask("root", call, result), serverResultUpdate(delta, "tool_call_result.server.serialized_result"),
		integrityTaskMessages(integrityMCP(t, "client", "mcp", "lookup", nil, nil)), testDone())
	parsed, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil {
		t.Fatal(err)
	}
	root, err := decodeContinuationState(parsed.RootTaskContext)
	if err != nil || root.GetMessages()[1].GetToolCallResult().GetToolCallId() != "call" || root.GetMessages()[1].GetToolCallResult().GetServer().GetSerializedResult() != "after" {
		t.Fatal("masked result lost original identity or payload update")
	}
	for _, path := range []string{"unknown", "task_id", "tool_call_result.unrecognized"} {
		stream = integrityStream(t, compatRootTask("root", call, result), serverResultUpdate(delta, path), integrityTaskMessages(continuationText("answer", "OK")), testDone())
		got, err := readStream(strings.NewReader(stream), "model-a")
		if path == "task_id" {
			if !errors.Is(err, errStreamStream) {
				t.Fatal("identity mask accepted")
			}
		} else if err != nil || got.Text != "OK" {
			t.Fatalf("official unknown mask no-op rejected: %s", path)
		}
	}
	// Appending individually bounded strings must still respect the total limit.
	large := officialServerResult("result", "call", strings.Repeat("x", serverBookkeepingPayloadLimit))
	appendAction := testAction(warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{
		TaskId: proto.String("root"), Message: officialServerResult("result", "", "x"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"tool_call_result.server.serialized_result"}},
	}.Build()}.Build())
	stream = integrityStream(t, compatRootTask("root", call, large), appendAction, testDone())
	parsed, err = readStream(strings.NewReader(stream), "model-a")
	if !errors.Is(err, errStreamLimit) || parsed.Rejection == nil || parsed.Rejection.PayloadBytes != serverBookkeepingPayloadLimit+1 {
		t.Fatal("masked append bypassed result byte bound")
	}
}

func TestServerResultMaskedTextDeltaCannotBecomeAnswer(t *testing.T) {
	call := officialServerCall("call", "call", "")
	result := officialServerResult("result", "call", "opaque")
	delta := continuationText("result", "phantom-answer")
	for _, paths := range [][]string{{"server_message_data"}, {"not_a_real_mask"}} {
		stream := integrityStream(t, compatRootTask("root", call, result), serverResultUpdate(delta, paths...), testDone())
		got, err := readStream(strings.NewReader(stream), "model-a")
		if err == nil || got.Text != "" || got.TextBytes != 0 {
			t.Fatal("masked ignored text became visible output")
		}
	}
}
