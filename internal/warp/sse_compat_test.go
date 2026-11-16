package warp

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func compatSSE(t *testing.T, label string, event *warpapi.ResponseEvent, padded, quoted bool) string {
	t.Helper()
	raw, err := proto.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	encoding := base64.RawURLEncoding
	if padded {
		encoding = base64.URLEncoding
	}
	data := encoding.EncodeToString(raw)
	if quoted {
		data = `"` + data + `"`
	}
	return "event: " + label + "\ndata: " + data + "\n\n"
}

func compatRootTask(id string, messages ...*warpapi.Message) *warpapi.ResponseEvent {
	return testAction(warpapi.ClientAction_builder{
		CreateTask: warpapi.ClientAction_CreateTask_builder{
			Task: warpapi.Task_builder{Id: &id, Messages: messages}.Build(),
		}.Build(),
	}.Build())
}

func compatServerData(id, data string) *warpapi.ResponseEvent {
	return testAction(warpapi.ClientAction_builder{
		UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{
			TaskId: &id, ServerData: &data,
		}.Build(),
	}.Build())
}

func TestStreamSSECompatibilityLabels(t *testing.T) {
	for _, labels := range [][]string{
		{"init", "client_actions", "finished"},
		{"custom-init", "custom-actions", "custom-finished"},
		{"", "message", "message"},
	} {
		t.Run(strings.Join(labels, "/"), func(t *testing.T) {
			stream := compatSSE(t, labels[0], warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{}.Build()}.Build(), false, false)
			stream += "event: ping\n\nevent: arbitrary-keepalive\n: heartbeat\n\n"
			stream += compatSSE(t, labels[1], compatRootTask("root", testOutput("answer", "OK")), false, false)
			stream += compatSSE(t, labels[2], testDone(), false, false)
			result, err := readStream(strings.NewReader(stream), "model-a")
			if err != nil || !result.Complete || result.Text != "OK" || result.Events != 3 {
				t.Fatalf("typed SSE labels rejected: result=%+v, err=%v", result, err)
			}
		})
	}
}

func TestStreamSSECompatibilityEncoding(t *testing.T) {
	for _, padded := range []bool{false, true} {
		for _, quoted := range []bool{false, true} {
			name := "raw"
			if padded {
				name += "/padded"
			}
			if quoted {
				name += "/quoted"
			}
			t.Run(name, func(t *testing.T) {
				// Empty Init serializes to two bytes, exercising actual '=' padding.
				stream := compatSSE(t, "init", warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{}.Build()}.Build(), padded, quoted)
				stream += compatSSE(t, "client_actions", testMessages(testOutput("m", "OK")), padded, quoted)
				stream += compatSSE(t, "finished", testDone(), padded, quoted)
				result, err := readStream(strings.NewReader(stream), "model-a")
				if err != nil || !result.Complete || result.Text != "OK" {
					t.Fatalf("base64url form rejected: result=%+v, err=%v", result, err)
				}
			})
		}
	}
}

func TestStreamSSECompatibilityQuoteTrimmingContract(t *testing.T) {
	init := warpapi.ResponseEvent_builder{Init: warpapi.ResponseEvent_StreamInit_builder{}.Build()}.Build()
	for _, quotes := range []struct{ before, after string }{
		{`"`, `"`}, {`""`, `""`}, {`"`, ""}, {"", `"`},
	} {
		// Rust trim_matches('"') removes edge quotes without parsing JSON.
		frame := testSSE(t, init)
		frame = strings.Replace(frame, "data: ", "data: "+quotes.before, 1)
		frame = strings.TrimSuffix(frame, "\n\n") + quotes.after + "\n\n"
		stream := frame + testSSE(t, testMessages(testOutput("m", "OK"))) + testSSE(t, testDone())
		result, err := readStream(strings.NewReader(stream), "model-a")
		if err != nil || !result.Complete || result.Text != "OK" {
			t.Fatalf("official edge-quote trimming rejected: result=%+v, err=%v", result, err)
		}
	}
}

func TestStreamSSECompatibilityBOM(t *testing.T) {
	stream := "\ufeff" + compatSSE(t, "error", testDone(), false, false)
	if _, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamUpstream) {
		t.Fatalf("BOM hid explicit error label: %v", err)
	}
	// A BOM before the first data field must not silently drop that message.
	stream = "\ufeff" + testSSE(t, testMessages(testOutput("m", "OK"))) + testSSE(t, testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if err != nil || !result.Complete || result.Text != "OK" {
		t.Fatalf("initial UTF-8 BOM rejected: result=%+v, err=%v", result, err)
	}
}

func TestStreamSSECompatibilityRejectsMalformedDataAndErrors(t *testing.T) {
	validSuffix := testSSE(t, testMessages(testOutput("m", "OK"))) + testSSE(t, testDone())
	for _, data := range []string{
		"", "not-base64!", `{"data":"CgA="}`, `["CgA="]`, `"CgA=\n"`, "CgB=", "CgA===", "____", "CgA+", "\ufeffCgA=",
	} {
		t.Run(data, func(t *testing.T) {
			_, err := readStream(strings.NewReader("event: arbitrary\ndata: "+data+"\n\n"+validSuffix), "model-a")
			if !errors.Is(err, errStreamStream) {
				t.Fatalf("malformed/JSON data accepted or wrong error: %v", err)
			}
		})
	}
	for _, frame := range []string{
		"event: error\n\n",
		"event: error\ndata: not-base64!\n\n",
		compatSSE(t, "error", testDone(), false, false),
	} {
		if _, err := readStream(strings.NewReader(frame+validSuffix), "model-a"); !errors.Is(err, errStreamUpstream) {
			t.Fatalf("explicit error label accepted or wrong error: %v", err)
		}
	}
}

func TestStreamSSECompatibilityLabelsStayBounded(t *testing.T) {
	stream := "event: " + strings.Repeat("x", frameLimit) + "\n\n"
	if _, err := readStream(strings.NewReader(stream), "model-a"); !errors.Is(err, errStreamLimit) {
		t.Fatalf("oversized no-data label accepted: %v", err)
	}
}

func TestStreamServerDataForBoundRootIsDiscarded(t *testing.T) {
	baseline := testSSE(t, compatRootTask("root", testOutput("m", "OK")))
	want, err := readStream(strings.NewReader(baseline+testSSE(t, testDone())), "model-a")
	if err != nil {
		t.Fatal(err)
	}
	// This is a synthetic opaque payload, not an executable message or JSON
	// envelope. Its entire contents must remain absent from the result.
	stream := baseline + compatSSE(t, "client_actions", compatServerData("root", "opaque-fixture: not interpreted"), false, false) + testSSE(t, testDone())
	got, err := readStream(strings.NewReader(stream), "model-a")
	want.Events++
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("root bookkeeping changed result: got=%+v, want=%+v, err=%v", got, want, err)
	}
}

func TestStreamServerDataRejectsUnboundAndForeignTasks(t *testing.T) {
	prefix := testSSE(t, compatRootTask("root", testOutput("m", "private partial")))
	for name, stream := range map[string]string{
		"unbound":         testSSE(t, compatServerData("root", "opaque")) + prefix,
		"foreign":         prefix + testSSE(t, compatServerData("other", "opaque")),
		"missing_task_id": prefix + testSSE(t, compatServerData("", "opaque")),
		"second_root":     prefix + testSSE(t, compatRootTask("other")),
		"duplicate_root":  prefix + testSSE(t, compatRootTask("root")),
	} {
		t.Run(name, func(t *testing.T) {
			result, err := readStream(strings.NewReader(stream+testSSE(t, testDone())), "model-a")
			if !errors.Is(err, errStreamTool) || result.Complete || result.Text != "" {
				t.Fatalf("task boundary accepted: result=%+v, err=%v", result, err)
			}
		})
	}
}

func TestStreamSSECompatibilityUnknownFieldsStillRejected(t *testing.T) {
	unknown := protowire.AppendTag(nil, 50000, protowire.VarintType)
	unknown = protowire.AppendVarint(unknown, 1)
	eventUnknown := testMessages(testOutput("m", "OK"))
	eventUnknown.ProtoReflect().SetUnknown(unknown)
	messageUnknown := testOutput("m", "OK")
	messageUnknown.GetAgentOutput().ProtoReflect().SetUnknown(unknown)
	actionUnknown := compatServerData("root", "opaque")
	actionUnknown.GetClientActions().GetActions()[0].GetUpdateTaskServerData().ProtoReflect().SetUnknown(unknown)
	for name, event := range map[string]*warpapi.ResponseEvent{
		"event": eventUnknown, "nested_message": testMessages(messageUnknown), "server_data": actionUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			stream := testSSE(t, compatRootTask("root")) + compatSSE(t, "client_actions", event, true, true) + testSSE(t, testDone())
			result, err := readStream(strings.NewReader(stream), "model-a")
			if !errors.Is(err, errStreamStream) || result.Complete || result.Text != "" {
				t.Fatalf("unknown fields accepted: result=%+v, err=%v", result, err)
			}
		})
	}
}

func TestStreamSSECompatibilityLabelsDoNotPermitExecution(t *testing.T) {
	for name, message := range map[string]*warpapi.Message{
		"tool":  warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{}.Build()}.Build(),
		"shell": warpapi.Message_builder{ToolCall: warpapi.Message_ToolCall_builder{RunShellCommand: warpapi.Message_ToolCall_RunShellCommand_builder{}.Build()}.Build()}.Build(),
		"agent": warpapi.Message_builder{MessagesReceivedFromAgents: warpapi.Message_MessagesReceivedFromAgents_builder{}.Build()}.Build(),
	} {
		t.Run(name, func(t *testing.T) {
			stream := compatSSE(t, "client_actions", testMessages(message), false, false)
			stream += testSSE(t, testMessages(testOutput("m", "OK"))) + testSSE(t, testDone())
			result, err := readStream(strings.NewReader(stream), "model-a")
			if !errors.Is(err, errStreamTool) || !result.ToolRequested || result.Complete || result.Text != "" {
				t.Fatalf("execution accepted under named label: result=%+v, err=%v", result, err)
			}
		})
	}
}

func TestStreamSSECompatibilityMetadataOnlyUpdateStillRejected(t *testing.T) {
	id, task, metadata := "m", "root", "opaque"
	update := testAction(warpapi.ClientAction_builder{
		UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{
			TaskId:  &task,
			Message: warpapi.Message_builder{Id: &id, TaskId: &task, ServerMessageData: &metadata}.Build(),
			Mask:    &fieldmaskpb.FieldMask{Paths: []string{"server_message_data"}},
		}.Build(),
	}.Build())
	stream := testSSE(t, compatRootTask(task, testOutput(id, "private partial"))) + testSSE(t, update) + testSSE(t, testDone())
	result, err := readStream(strings.NewReader(stream), "model-a")
	if !errors.Is(err, errStreamStream) || result.Complete || result.Text != "" {
		t.Fatalf("metadata-only fieldmask unexpectedly accepted: result=%+v, err=%v", result, err)
	}
}
