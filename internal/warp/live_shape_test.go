package warp

import (
	"encoding/base64"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

// liveShapeStream reproduces the ordered event sequence captured from one real
// Warp response: init, root task with a server bootstrap tool, user query,
// opaque server state, model bookkeeping, answer, request metadata carrying
// platform credits, and a Done finish with request charges.
func liveShapeStream(t *testing.T) string {
	t.Helper()
	id, task, query := "user-1", "root", "Reply exactly OK."
	bootstrap := warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{
		Server: warpapi.Message_ToolCall_Server_builder{Payload: proto.String("")}.Build(),
	}.Build()}.Build()
	user := warpapi.Message_builder{Id: &id, TaskId: &task, UserQuery: warpapi.Message_UserQuery_builder{Query: &query}.Build()}.Build()
	serverEvent := warpapi.Message_builder{TaskId: &task, ServerEvent: warpapi.Message_ServerEvent_builder{Payload: proto.String(`{"opaque":true}`)}.Build()}.Build()
	modelUsed := warpapi.Message_builder{TaskId: &task, ModelUsed: warpapi.Message_ModelUsed_builder{ModelId: proto.String("model-a")}.Build()}.Build()
	charges := warpapi.RequestCharges_builder{UsageByCategory: map[string]*warpapi.ChargedUsage{
		"primary_agent": warpapi.ChargedUsage_builder{PlatformUsageInCredits: proto.Float32(1.5)}.Build(),
	}}.Build()
	metadata := warpapi.Message_builder{TaskId: &task, RequestMetadata: warpapi.Message_RequestMetadata_builder{
		Charges: charges, Outcome: warpapi.Message_RequestMetadata_OUTCOME_COMPLETED.Enum(),
	}.Build()}.Build()
	data, _ := proto.Marshal(warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{
		ConversationId: proto.String("conversation"), RequestId: proto.String("request"), RunId: proto.String("run"),
	}.Build()}.Build())
	stream := "event: init\ndata: " + encodeFrame(data) + "\n\n"
	for _, event := range []*warpapi.ResponseEvent{
		compatRootTask(task, bootstrap),
		testMessages(serverEvent, user),
		compatServerData(task, "opaque-state"),
		testAction(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: &task, Message: user}.Build()}.Build()),
		testAction(warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build()),
		testMessages(modelUsed),
		testMessages(testOutput("answer", "OK")),
		testMessages(serverEvent),
		testAction(warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build()),
		testAction(warpapi.ClientAction_builder{UpdateTaskDescription: warpapi.ClientAction_UpdateTaskDescription_builder{TaskId: &task, Description: proto.String("desc")}.Build()}.Build()),
		testMessages(metadata),
		warpapi.ResponseEvent_builder{Finished: warpapi.ResponseEvent_StreamFinished_builder{
			Done: warpapi.ResponseEvent_StreamFinished_Done_builder{}.Build(), RequestCharges: charges,
		}.Build()}.Build(),
	} {
		raw, err := proto.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		// Real frames carried non-canonical SSE labels.
		stream += "event: client_actions\ndata: " + encodeFrame(raw) + "\n\n"
	}
	return stream
}

func TestStreamAcceptsObservedLiveShape(t *testing.T) {
	result, err := readStream(strings.NewReader(liveShapeStream(t)), "model-a")
	if err != nil || !result.Complete || result.Text != "OK" || result.ToolRequested {
		t.Fatalf("observed live shape rejected: result=%+v, err=%v", result, err)
	}
	if result.ServerBookkeeping < 2 {
		t.Fatalf("opaque server bookkeeping not counted: %+v", result)
	}
	if !result.Usage.Measured || result.Usage.PlatformCredits == nil || *result.Usage.PlatformCredits != 1.5 {
		t.Fatalf("new charging metadata not observed: %+v", result.Usage)
	}
}

func encodeFrame(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
