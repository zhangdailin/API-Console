package warp

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func integrityMCP(t *testing.T, messageID, callID, name string, args map[string]any, serverID *string) *warpapi.Message {
	t.Helper()
	values, err := structpb.NewStruct(args)
	if err != nil {
		t.Fatal(err)
	}
	return warpapi.Message_builder{Id: proto.String(messageID), ToolCall: warpapi.Message_ToolCall_builder{
		ToolCallId: proto.String(callID),
		CallMcpTool: warpapi.Message_ToolCall_CallMCPTool_builder{
			Name: proto.String(name), Args: values, ServerId: serverID,
		}.Build(),
	}.Build()}.Build()
}

func integrityCharges() *warpapi.RequestCharges {
	return warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
		"primary_agent": warpapi.ChargedUsage_builder{PlatformUsageInCredits: proto.Float32(1.5)}.Build(),
	}}.Build()
}

func integrityMetadata(summary *warpapi.Message_RequestMetadata_ToolCallSummary, charges *warpapi.RequestCharges) *warpapi.Message {
	return warpapi.Message_builder{Id: proto.String("metadata"), RequestMetadata: warpapi.Message_RequestMetadata_builder{
		ToolCallSummary: summary, Charges: charges,
		Outcome: warpapi.Message_RequestMetadata_OUTCOME_COMPLETED.Enum(),
	}.Build()}.Build()
}

func integrityTaskMessages(messages ...*warpapi.Message) *warpapi.ResponseEvent {
	return testAction(warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{
		TaskId: proto.String("root"), Messages: messages,
	}.Build()}.Build())
}

func integrityStream(t *testing.T, events ...*warpapi.ResponseEvent) string {
	t.Helper()
	var stream strings.Builder
	for _, event := range events {
		stream.WriteString(testSSE(t, event))
	}
	return stream.String()
}

func TestToolIntegrityAcceptsValidatedMCPSummary(t *testing.T) {
	stream := integrityStream(t,
		compatRootTask("root", integrityMCP(t, "message-1", "call-1", "lookup", map[string]any{"q": "x"}, proto.String("client-request-tools"))),
		integrityTaskMessages(integrityMetadata(warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(1)}.Build(), integrityCharges())),
		testDone(),
	)
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || result.ToolRequested || len(result.ToolCalls) != 1 {
		t.Fatalf("validated MCP summary rejected: result=%+v err=%v", result, err)
	}
	if !result.Usage.Measured || result.Usage.PlatformCredits == nil || *result.Usage.PlatformCredits != 1.5 || result.Usage.Source != "request_charges" {
		t.Fatalf("metadata charges lost at unmeasured Done: %+v", result.Usage)
	}
}

func TestToolIntegrityRejectsUnexpectedSummaryAndPreservesCharges(t *testing.T) {
	cases := map[string]*warpapi.Message_RequestMetadata_ToolCallSummary{
		"extra tool": warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(2)}.Build(),
		"shell":      warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(1), CommandsExecuted: proto.Uint32(1)}.Build(),
		"file":       warpapi.Message_RequestMetadata_ToolCallSummary_builder{FilesChanged: proto.Uint32(1)}.Build(),
		"added":      warpapi.Message_RequestMetadata_ToolCallSummary_builder{LinesAdded: proto.Uint32(1)}.Build(),
		"removed":    warpapi.Message_RequestMetadata_ToolCallSummary_builder{LinesRemoved: proto.Uint32(1)}.Build(),
	}
	for name, summary := range cases {
		t.Run(name, func(t *testing.T) {
			stream := integrityStream(t,
				compatRootTask("root", integrityMCP(t, "message-1", "call-1", "lookup", nil, nil)),
				integrityTaskMessages(integrityMetadata(summary, integrityCharges())), testDone(),
			)
			result, err := readStream(strings.NewReader(stream), "model-a")
			if !errors.Is(err, errStreamTool) || result.Complete || !result.ToolRequested {
				t.Fatalf("unsafe summary accepted: result=%+v err=%v", result, err)
			}
			if result.Usage.PlatformCredits == nil || *result.Usage.PlatformCredits != 1.5 {
				t.Fatalf("rejection discarded charges: %+v", result.Usage)
			}
		})
	}
	stream := integrityStream(t, testMessages(testOutput("answer", "OK")),
		testMessages(integrityMetadata(warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(1)}.Build(), nil)), testDone())
	if result, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamTool) || result.Complete {
		t.Fatalf("unobserved tool count accepted: result=%+v err=%v", result, err)
	}
}

func TestToolIntegrityDeduplicatesAddAndUpdateSnapshots(t *testing.T) {
	first := integrityMCP(t, "message-1", "call-1", "lookup", map[string]any{"z": 1, "a": map[string]any{"y": 2, "b": 3}}, nil)
	replay := integrityMCP(t, "message-1", "call-1", "lookup", map[string]any{"a": map[string]any{"b": 3, "y": 2}, "z": 1}, proto.String("client-request-tools"))
	stream := integrityStream(t,
		compatRootTask("root"),
		integrityTaskMessages(first),
		testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: replay}.Build()}.Build()),
		integrityTaskMessages(integrityMetadata(warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(1)}.Build(), nil)), testDone(),
	)
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || len(result.ToolCalls) != 1 {
		t.Fatalf("duplicate emitted more than once: result=%+v err=%v", result, err)
	}
	if result.ToolCalls[0].Args != `{"a":{"b":3,"y":2},"z":1}` {
		t.Fatalf("arguments not canonical: %+v", result.ToolCalls[0])
	}
}

func TestToolIntegrityRejectsConflictingCallID(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"different-name", map[string]any{"q": "x"}},
		{"lookup", map[string]any{"q": "changed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := integrityStream(t,
				compatRootTask("root", integrityMCP(t, "message-1", "call-1", "lookup", map[string]any{"q": "x"}, nil)),
				testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{
					TaskId: proto.String("root"), Message: integrityMCP(t, "message-1", "call-1", tc.name, tc.args, nil),
				}.Build()}.Build()), testDone(),
			)
			if result, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamTool) || result.Complete || !result.ToolRequested {
				t.Fatalf("conflicting replay accepted: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestToolIntegrityRollbackRestoresCallsAndIdentity(t *testing.T) {
	begin := testAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build())
	rollback := testAction(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build())
	base := []*warpapi.ResponseEvent{
		compatRootTask("root", integrityMCP(t, "kept", "kept-call", "lookup", nil, nil)), begin,
		integrityTaskMessages(integrityMCP(t, "discarded", "discarded-call", "lookup", nil, nil)), rollback,
	}
	stream := integrityStream(t, append(base, testDone())...)
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "kept-call" {
		t.Fatalf("rollback retained executable call: result=%+v err=%v", result, err)
	}
	// Rolled-back IDs are removed from replay state as well as the emitted list.
	stream = integrityStream(t, append(base,
		integrityTaskMessages(integrityMCP(t, "replacement", "discarded-call", "replacement", nil, nil)), testDone())...)
	if result, err := readStream(strings.NewReader(stream), "model-a"); err != nil || len(result.ToolCalls) != 2 || result.ToolCalls[1].Name != "replacement" {
		t.Fatalf("rollback left stale identity: result=%+v err=%v", result, err)
	}
	stream = integrityStream(t, append(base,
		integrityTaskMessages(integrityMetadata(warpapi.Message_RequestMetadata_ToolCallSummary_builder{ToolCalls: proto.Uint32(2)}.Build(), nil)), testDone())...)
	if result, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamTool) || result.Complete {
		t.Fatalf("summary counted rolled-back call: result=%+v err=%v", result, err)
	}
}

func TestToolIntegrityRejectsMissingIDAndForeignServer(t *testing.T) {
	for name, call := range map[string]*warpapi.Message{
		"missing ID":     integrityMCP(t, "message-1", "", "lookup", nil, nil),
		"foreign server": integrityMCP(t, "message-1", "call-1", "lookup", nil, proto.String("foreign")),
		"empty server":   integrityMCP(t, "message-1", "call-1", "lookup", nil, proto.String("")),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := readStream(strings.NewReader(integrityStream(t, testMessages(call), testDone())), "model-a")
			if !errors.Is(err, errStreamTool) || result.Complete || len(result.ToolCalls) != 0 || !result.ToolRequested {
				t.Fatalf("invalid identity accepted: result=%+v err=%v", result, err)
			}
		})
	}
}

func TestToolIntegrityLimitsUniqueEmittedCalls(t *testing.T) {
	events := []*warpapi.ResponseEvent{compatRootTask("root")}
	for i := 0; i < 6; i++ {
		id := "call-" + strconv.Itoa(i)
		events = append(events, integrityTaskMessages(integrityMCP(t, id, id, "lookup", nil, nil)))
	}
	// Replays do not consume another call slot, even at the limit.
	events = append(events, testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{
		TaskId: proto.String("root"), Message: integrityMCP(t, "call-0", "call-0", "lookup", nil, nil),
	}.Build()}.Build()))
	stream := integrityStream(t, append(events, testDone())...)
	if result, err := readStream(strings.NewReader(stream), "model-a"); err != nil || len(result.ToolCalls) != 6 || !result.Complete {
		t.Fatalf("six unique calls rejected: result=%+v err=%v", result, err)
	}
	stream = integrityStream(t, append(events, integrityTaskMessages(integrityMCP(t, "seventh", "call-7", "lookup", nil, nil)), testDone())...)
	if result, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamTool) || result.Complete || len(result.ToolCalls) != 6 {
		t.Fatalf("seven unique calls accepted: result=%+v err=%v", result, err)
	}
}

func TestToolIntegrityOtherFinishPreservesMetadataCharges(t *testing.T) {
	other := warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{
		Other: warpapi.ResponseEvent_StreamFinished_Other_builder{}.Build(),
	}.Build()}.Build()
	stream := integrityStream(t, compatRootTask("root", integrityMCP(t, "message-1", "call-1", "lookup", nil, nil)),
		integrityTaskMessages(integrityMetadata(nil, integrityCharges())), other)
	result, err := readStream(strings.NewReader(stream), "model-a")
	if !errors.Is(err, errStreamUpstream) || result.Complete || result.Usage.PlatformCredits == nil || *result.Usage.PlatformCredits != 1.5 {
		t.Fatalf("Other finish accepted or discarded charges: result=%+v err=%v", result, err)
	}
}
