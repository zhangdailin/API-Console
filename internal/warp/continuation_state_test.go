package warp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/middleware"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func continuationText(id, text string) *warpapi.Message {
	return warpapi.Message_builder{Id: proto.String(id), TaskId: proto.String("root"), AgentOutput: warpapi.Message_AgentOutput_builder{Text: proto.String(text)}.Build()}.Build()
}

func continuationCreate(messages ...*warpapi.Message) *warpapi.ClientAction {
	return warpapi.ClientAction_builder{CreateTask: warpapi.ClientAction_CreateTask_builder{Task: warpapi.Task_builder{Id: proto.String("root"), Messages: messages}.Build()}.Build()}.Build()
}

func continuationRoot(t *testing.T, a *continuationAccumulator) *warpapi.Task {
	t.Helper()
	raw, err := a.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	root, err := decodeContinuationState(raw)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func continuationApply(t *testing.T, a *continuationAccumulator, action *warpapi.ClientAction) {
	t.Helper()
	if err := a.apply(action); err != nil {
		t.Fatal(err)
	}
}

func TestContinuationAccumulatorRealSequence(t *testing.T) {
	a := newContinuationAccumulator(nil)
	create := continuationCreate(continuationText("out", "first"))
	continuationApply(t, a, create)
	// Inputs are never retained by reference.
	create.GetCreateTask().GetTask().GetMessages()[0].GetAgentOutput().SetText("mutated")
	continuationApply(t, a, warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{TaskId: proto.String("root"), Message: continuationText("out", " second"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}}.Build()}.Build())
	continuationApply(t, a, warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{TaskId: proto.String("root"), ServerData: proto.String("opaque-state")}.Build()}.Build())
	continuationApply(t, a, warpapi.ClientAction_builder{UpdateTaskDescription: warpapi.ClientAction_UpdateTaskDescription_builder{TaskId: proto.String("root"), Description: proto.String("description")}.Build()}.Build())
	continuationApply(t, a, warpapi.ClientAction_builder{UpdateTaskSummary: warpapi.ClientAction_UpdateTaskSummary_builder{TaskId: proto.String("root"), Summary: proto.String("summary")}.Build()}.Build())
	tool := warpapi.Message_builder{Id: proto.String("tool-message"), TaskId: proto.String("root"), ToolCall: warpapi.Message_ToolCall_builder{ToolCallId: proto.String("call-1"), Server: warpapi.Message_ToolCall_Server_builder{Payload: proto.String("opaque-tool")}.Build()}.Build()}.Build()
	continuationApply(t, a, warpapi.ClientAction_builder{AddMessagesToTask: warpapi.ClientAction_AddMessagesToTask_builder{TaskId: proto.String("root"), Messages: []*warpapi.Message{tool}}.Build()}.Build())
	root := continuationRoot(t, a)
	if root.GetMessages()[0].GetAgentOutput().GetText() != "first second" || root.GetServerData() != "opaque-state" || root.GetDescription() != "description" || root.GetSummary() != "summary" || !proto.Equal(root.GetMessages()[1], tool) {
		t.Fatal("official continuation state was not faithfully reconstructed")
	}
	raw, err := a.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	resumed := newContinuationAccumulator(raw)
	continuationApply(t, resumed, warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: continuationText("out", "replacement"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}}.Build()}.Build())
	if continuationRoot(t, resumed).GetMessages()[0].GetAgentOutput().GetText() != "replacement" || continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText() != "first second" {
		t.Fatal("replacement or continuation ownership failed")
	}
}

func TestContinuationTransactionsAndMasks(t *testing.T) {
	a := newContinuationAccumulator(nil)
	continuationApply(t, a, continuationCreate(continuationText("out", "before")))
	begin := warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build()
	continuationApply(t, a, begin)
	if _, err := a.snapshot(); err == nil {
		t.Fatal("uncommitted transaction exported")
	}
	continuationApply(t, a, warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: continuationText("out", "during"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}}.Build()}.Build())
	continuationApply(t, a, warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build())
	if continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText() != "before" {
		t.Fatal("rollback did not restore full snapshot")
	}
	continuationApply(t, a, begin)
	continuationApply(t, a, warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{TaskId: proto.String("root"), Message: continuationText("out", " committed"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}}.Build()}.Build())
	continuationApply(t, a, warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build())
	if continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText() != "before committed" {
		t.Fatal("commit lost appended content")
	}
}

func TestContinuationRejectsInvalidAndOversizedState(t *testing.T) {
	tests := map[string]func(*continuationAccumulator) error{
		"second task": func(a *continuationAccumulator) error { return a.apply(continuationCreate()) },
		"unknown field": func(a *continuationAccumulator) error {
			x := warpapi.ClientAction_builder{UpdateTaskSummary: warpapi.ClientAction_UpdateTaskSummary_builder{TaskId: proto.String("root")}.Build()}.Build()
			x.ProtoReflect().SetUnknown([]byte{0xa0, 0x06, 0x01})
			return a.apply(x)
		},
		"wrong task": func(a *continuationAccumulator) error {
			return a.apply(warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{TaskId: proto.String("other")}.Build()}.Build())
		},
		"oversized": func(a *continuationAccumulator) error {
			return a.apply(warpapi.ClientAction_builder{UpdateTaskServerData: warpapi.ClientAction_UpdateTaskServerData_builder{TaskId: proto.String("root"), ServerData: proto.String(strings.Repeat("x", continuationStateLimit))}.Build()}.Build())
		},
		"missing message": func(a *continuationAccumulator) error {
			return a.apply(warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{TaskId: proto.String("root"), Message: continuationText("missing", "x"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}}.Build()}.Build())
		},
		"invalid mask": func(a *continuationAccumulator) error {
			return a.apply(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: continuationText("out", "x"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text.invalid"}}}.Build()}.Build())
		},
		"identity mask": func(a *continuationAccumulator) error {
			return a.apply(warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: continuationText("out", "x"), Mask: &fieldmaskpb.FieldMask{Paths: []string{"task_id"}}}.Build()}.Build())
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			a := newContinuationAccumulator(nil)
			continuationApply(t, a, continuationCreate(continuationText("out", "before")))
			if run(a) == nil {
				t.Fatal("invalid action accepted")
			}
			if _, err := a.snapshot(); err == nil {
				t.Fatal("invalid state exported")
			}
		})
	}
	for _, initial := range [][]byte{{0xff}, bytes.Repeat([]byte("x"), continuationStateLimit+1)} {
		if _, err := newContinuationAccumulator(initial).snapshot(); err == nil {
			t.Fatal("invalid initial state accepted")
		}
	}
}

func bindingAuthContext(t *testing.T, id int64, key string) context.Context {
	t.Helper()
	var ctx context.Context
	handler := middleware.APIKeyAuth(func(context.Context, string) (*middleware.APIKeyPrincipal, error) {
		return &middleware.APIKeyPrincipal{ID: id}, nil
	}, func(_ http.ResponseWriter, r *http.Request) { ctx = r.Context() })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	handler(httptest.NewRecorder(), r)
	if ctx == nil {
		t.Fatal("test authentication failed")
	}
	return ctx
}

type continuationMemoryRedis struct {
	values map[string][]byte
	ttls   map[string]time.Duration
}

func (r *continuationMemoryRedis) SetBinding(_ context.Context, key string, value []byte, ttl time.Duration) error {
	r.values[key] = bytes.Clone(value)
	r.ttls[key] = ttl
	return nil
}
func (r *continuationMemoryRedis) GetBinding(_ context.Context, key string) ([]byte, error) {
	return bytes.Clone(r.values[key]), nil
}

func TestScopedBindingsIsolationAndPersistedValidation(t *testing.T) {
	ctx := bindingAuthContext(t, 1, "unit-key-one")
	other := bindingAuthContext(t, 2, "unit-key-two")
	rotated := bindingAuthContext(t, 1, "unit-key-rotated")
	a := newContinuationAccumulator(nil)
	continuationApply(t, a, continuationCreate(continuationText("out", "private history")))
	raw, _ := a.snapshot()
	backend := &continuationMemoryRedis{values: map[string][]byte{}, ttls: map[string]time.Duration{}}
	b := newBindingStore()
	b.redis = backend
	input := callBinding{ConversationID: "conversation", RootTaskContext: raw, ToolRounds: 2}
	if !b.saveScoped(ctx, 7, "model-a", "same-call", input) {
		t.Fatal("scoped save failed")
	}
	for name, lookup := range map[string]func() bool{
		"other principal": func() bool { _, ok := b.lookupScoped(other, 7, "model-a", []string{"same-call"}); return ok },
		"rotated key":     func() bool { _, ok := b.lookupScoped(rotated, 7, "model-a", []string{"same-call"}); return ok },
		"other account":   func() bool { _, ok := b.lookupScoped(ctx, 8, "model-a", []string{"same-call"}); return ok },
		"other model":     func() bool { _, ok := b.lookupScoped(ctx, 7, "model-b", []string{"same-call"}); return ok },
		"missing principal": func() bool {
			_, ok := b.lookupScoped(context.Background(), 7, "model-a", []string{"same-call"})
			return ok
		},
	} {
		if lookup() {
			t.Fatalf("binding escaped %s scope", name)
		}
	}
	input.RootTaskContext[0] ^= 0xff
	found, ok := b.lookupScoped(ctx, 7, "model-a", []string{"same-call"})
	if !ok {
		t.Fatal("input mutation changed cached binding")
	}
	found.RootTaskContext[0] ^= 0xff
	if _, ok := b.lookupScoped(ctx, 7, "model-a", []string{"same-call"}); !ok {
		t.Fatal("output mutation changed cached binding")
	}
	fresh := newBindingStore()
	fresh.redis = backend
	if got, ok := fresh.lookupScoped(ctx, 7, "model-a", []string{"same-call"}); !ok || got.ToolRounds != 2 || got.AccountID != 7 {
		t.Fatal("cross-instance scoped lookup failed")
	}
	for key, value := range backend.values {
		if strings.Contains(key, "same-call") || strings.Contains(key, "model-a") || backend.ttls[key] != time.Hour {
			t.Fatal("persisted key or TTL violated contract")
		}
		var record callBinding
		if json.Unmarshal(value, &record) != nil {
			t.Fatal("invalid record")
		}
		record.OwnerScope = "foreign"
		backend.values[key], _ = json.Marshal(record)
	}
	if _, ok := fresh.lookupScoped(ctx, 7, "model-a", []string{"same-call"}); ok {
		t.Fatal("foreign persisted owner accepted")
	}
	if b.saveScoped(context.Background(), 7, "model-a", "x", callBinding{ConversationID: "c"}) {
		t.Fatal("keyless production binding accepted")
	}
	if b.saveScoped(ctx, 7, "model-a", "x", callBinding{ConversationID: "c", ToolRounds: maxToolRounds + 1}) {
		t.Fatal("excessive tool rounds accepted")
	}
	if b.saveScoped(ctx, 7, "model-a", "x", callBinding{ConversationID: "c", RootTaskContext: bytes.Repeat([]byte("x"), continuationStateLimit+1)}) {
		t.Fatal("oversized binding accepted")
	}
}

func TestContinuationMaskedNestedUpdateAndClear(t *testing.T) {
	a := newContinuationAccumulator(nil)
	tool := warpapi.Message_builder{Id: proto.String("tool"), TaskId: proto.String("root"), ToolCall: warpapi.Message_ToolCall_builder{ToolCallId: proto.String("call-1"), Server: warpapi.Message_ToolCall_Server_builder{Payload: proto.String("before")}.Build()}.Build()}.Build()
	continuationApply(t, a, continuationCreate(tool, continuationText("out", "remove")))
	delta := warpapi.Message_builder{Id: proto.String("tool"), ToolCall: warpapi.Message_ToolCall_builder{Server: warpapi.Message_ToolCall_Server_builder{Payload: proto.String("after")}.Build()}.Build()}.Build()
	continuationApply(t, a, warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: delta, Mask: &fieldmaskpb.FieldMask{Paths: []string{"tool_call.server.payload"}}}.Build()}.Build())
	delta.GetToolCall().GetServer().SetPayload("mutated")
	continuationApply(t, a, warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: continuationText("out", ""), Mask: &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}}.Build()}.Build())
	root := continuationRoot(t, a)
	if root.GetMessages()[0].GetToolCall().GetToolCallId() != "call-1" || root.GetMessages()[0].GetToolCall().GetServer().GetPayload() != "after" || root.GetMessages()[1].GetAgentOutput().GetText() != "" {
		t.Fatal("masked update lost unselected fields, aliased input, or failed to clear text")
	}
}

func TestContinuationRejectsNonRootAndMultipleInitialTasks(t *testing.T) {
	root := warpapi.Task_builder{Id: proto.String("root")}.Build()
	child := warpapi.Task_builder{Id: proto.String("child"), Dependencies: warpapi.Task_Dependencies_builder{ParentTaskId: proto.String("root")}.Build()}.Build()
	for _, tasks := range [][]*warpapi.Task{{}, {root, child}, {child}} {
		raw, err := proto.Marshal(warpapi.Request_TaskContext_builder{Tasks: tasks}.Build())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeContinuationState(raw); err == nil {
			t.Fatal("invalid task hierarchy accepted")
		}
	}
}

func TestScopedBindingExpiryAndMixedTurnSnapshots(t *testing.T) {
	ctx := bindingAuthContext(t, 1, "unit-key")
	b := newBindingStore()
	b.saveScoped(ctx, 7, "model-a", "first", callBinding{ConversationID: "c", ToolRounds: 1})
	b.saveScoped(ctx, 7, "model-a", "second", callBinding{ConversationID: "c", ToolRounds: 2})
	if _, ok := b.lookupScoped(ctx, 7, "model-a", []string{"first", "second"}); ok {
		t.Fatal("mixed parent depths accepted")
	}
	owner, _ := bindingOwner(ctx)
	id := scopedBindingID(owner, 7, "model-a", "first")
	b.mu.Lock()
	binding := b.memory[id]
	binding.SavedAt = time.Now().Add(-bindingTTL - time.Second)
	b.memory[id] = binding
	b.mu.Unlock()
	if _, ok := b.lookupScoped(ctx, 7, "model-a", []string{"first"}); ok {
		t.Fatal("expired scoped binding accepted")
	}
}

func continuationUpdate(message *warpapi.Message, mask *fieldmaskpb.FieldMask, appendOnly bool) *warpapi.ClientAction {
	if appendOnly {
		return warpapi.ClientAction_builder{AppendToMessageContent: warpapi.ClientAction_AppendToMessageContent_builder{TaskId: proto.String("root"), Message: message, Mask: mask}.Build()}.Build()
	}
	return warpapi.ClientAction_builder{UpdateTaskMessage: warpapi.ClientAction_UpdateTaskMessage_builder{TaskId: proto.String("root"), Message: message, Mask: mask}.Build()}.Build()
}

func requireContinuationError(t *testing.T, err error, reason string, kind int) *continuationStateError {
	t.Helper()
	var stateErr *continuationStateError
	if !errors.Is(err, errContinuationState) || !errors.As(err, &stateErr) {
		t.Fatalf("expected typed continuation error, got %T: %v", err, err)
	}
	if stateErr.Reason != reason || stateErr.ActionKind != kind || stateErr.LimitBytes != continuationStateLimit {
		t.Fatalf("unexpected continuation error metadata: %+v", stateErr)
	}
	return stateErr
}

func TestContinuationOfficialMaskPresenceAndUpsert(t *testing.T) {
	for _, appendOnly := range []bool{false, true} {
		t.Run(map[bool]string{false: "update", true: "append"}[appendOnly], func(t *testing.T) {
			a := newContinuationAccumulator(nil)
			continuationApply(t, a, continuationCreate(continuationText("out", "before")))
			for _, mask := range []*fieldmaskpb.FieldMask{nil, {}} {
				continuationApply(t, a, continuationUpdate(continuationText("out", "ignored"), mask, appendOnly))
				if got := continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText(); got != "before" {
					t.Fatalf("absent/empty mask changed existing message: %q", got)
				}
			}
			// Absent masks skip even a missing message and never create a new ID.
			continuationApply(t, a, continuationUpdate(nil, nil, appendOnly))
			continuationApply(t, a, continuationUpdate(continuationText("new", "ignored"), nil, appendOnly))
			if len(continuationRoot(t, a).GetMessages()) != 1 {
				t.Fatal("absent mask inserted a message")
			}
		})
	}
	for _, mask := range []*fieldmaskpb.FieldMask{{}, {Paths: []string{"agent_output.text"}}, {Paths: []string{"future.field"}}} {
		a := newContinuationAccumulator(nil)
		continuationApply(t, a, continuationCreate())
		message := continuationText("new", "full supplied message")
		message.SetServerMessageData("unselected but preserved")
		continuationApply(t, a, continuationUpdate(message, mask, false))
		if !proto.Equal(continuationRoot(t, a).GetMessages()[0], message) {
			t.Fatal("upsert did not insert the full supplied message")
		}
		message.GetAgentOutput().SetText("mutated")
		if continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText() != "full supplied message" {
			t.Fatal("upsert retained an input alias")
		}
	}
}

func TestContinuationUnknownMasksDefaultsAndOrderedDuplicates(t *testing.T) {
	a := newContinuationAccumulator(nil)
	continuationApply(t, a, continuationCreate(continuationText("out", "a")))
	before := proto.Clone(a.root)
	// Unknown nested fields must not materialize a parent or switch a oneof.
	continuationApply(t, a, continuationUpdate(continuationText("out", "ignored"), &fieldmaskpb.FieldMask{Paths: []string{"future.field", "tool_call.server.future"}}, false))
	if !proto.Equal(before, a.root) {
		t.Fatal("unknown path mutated state")
	}
	mask := &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text", "future.field", "agent_output.text"}}
	continuationApply(t, a, continuationUpdate(continuationText("out", "b"), mask, true))
	if got := continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText(); got != "abb" || len(mask.Paths) != 3 {
		t.Fatalf("duplicate append paths were normalized or aliased: %q", got)
	}
	// Parent/child update order matters. The absent source parent is a default,
	// not an invalid mask, and the selected text leaf clears to its default.
	continuationApply(t, a, continuationUpdate(warpapi.Message_builder{Id: proto.String("out")}.Build(), &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}, false))
	if got := continuationRoot(t, a).GetMessages()[0].GetAgentOutput().GetText(); got != "" {
		t.Fatalf("absent nested source did not clear selected leaf: %q", got)
	}
}

func TestContinuationMaskParentChildOrder(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		a := newContinuationAccumulator(nil)
		continuationApply(t, a, continuationCreate(continuationText("out", "before")))
		paths := []string{"agent_output.text", "agent_output"}
		if parentFirst {
			paths[0], paths[1] = paths[1], paths[0]
		}
		delta := warpapi.Message_builder{Id: proto.String("out")}.Build()
		continuationApply(t, a, continuationUpdate(delta, &fieldmaskpb.FieldMask{Paths: paths}, false))
		if got := continuationRoot(t, a).GetMessages()[0].HasAgentOutput(); got != parentFirst {
			t.Fatalf("mask order changed: parentFirst=%v hasAgentOutput=%v", parentFirst, got)
		}
	}
}

func TestContinuationRejectsUnsafeMasksAndUnknownUpserts(t *testing.T) {
	for _, path := range []string{"id", "task_id", "future.id", "tool_call.future.task_id", "agent_output..text", "", "agent_output.text.child", "request_metadata.charges.usage_by_category.key"} {
		t.Run(path, func(t *testing.T) {
			a := newContinuationAccumulator(nil)
			continuationApply(t, a, continuationCreate(continuationText("out", "before")))
			action := continuationUpdate(continuationText("out", "after"), &fieldmaskpb.FieldMask{Paths: []string{path}}, false)
			err := a.apply(action)
			reason := "invalid_mask"
			for _, part := range strings.Split(path, ".") {
				if part == "id" || part == "task_id" {
					reason = "identity_mutation"
				}
			}
			requireContinuationError(t, err, reason, int(action.WhichAction()))
			if strings.Contains(err.Error(), "before") || len(path) > 8 && strings.Contains(err.Error(), path) {
				t.Fatal("error included content or a mask path")
			}
		})
	}
	t.Run("foreign upsert", func(t *testing.T) {
		a := newContinuationAccumulator(nil)
		continuationApply(t, a, continuationCreate())
		message := continuationText("new", "content")
		message.SetTaskId("foreign")
		action := continuationUpdate(message, &fieldmaskpb.FieldMask{}, false)
		requireContinuationError(t, a.apply(action), "invalid_task", int(action.WhichAction()))
		if len(a.root.GetMessages()) != 0 {
			t.Fatal("foreign upsert was accepted")
		}
	})
	for _, unknown := range []string{"outer", "nested"} {
		t.Run(unknown, func(t *testing.T) {
			a := newContinuationAccumulator(nil)
			continuationApply(t, a, continuationCreate())
			message := continuationText("new", "private-payload")
			reflection := message.ProtoReflect()
			if unknown == "nested" {
				reflection = message.GetAgentOutput().ProtoReflect()
			}
			reflection.SetUnknown([]byte{0xa0, 0x06, 0x01})
			action := continuationUpdate(message, &fieldmaskpb.FieldMask{}, false)
			err := a.apply(action)
			requireContinuationError(t, err, "unknown_fields", int(action.WhichAction()))
			if len(a.root.GetMessages()) != 0 || strings.Contains(err.Error(), "private-payload") {
				t.Fatal("unknown full upsert was partially accepted or exposed")
			}
		})
	}
}

func continuationReceivedMessage(values ...string) *warpapi.Message {
	message := warpapi.Message_builder{Id: proto.String("out"), TaskId: proto.String("root")}.Build()
	reflection := message.ProtoReflect()
	field := reflection.Descriptor().Fields().ByName("messages_received_from_agents")
	parent := reflection.Mutable(field).Message()
	listField := parent.Descriptor().Fields().ByName("messages")
	list := parent.Mutable(listField).List()
	for _, value := range values {
		item := list.NewElement()
		body := item.Message().Descriptor().Fields().ByName("message_body")
		item.Message().Set(body, protoreflect.ValueOfString(value))
		list.Append(item)
	}
	return message
}

func TestContinuationRepeatedMessageMasksPairwise(t *testing.T) {
	path := "messages_received_from_agents.messages.message_body"
	for _, appendOnly := range []bool{false, true} {
		a := newContinuationAccumulator(nil)
		continuationApply(t, a, continuationCreate(continuationReceivedMessage("a", "b")))
		continuationApply(t, a, continuationUpdate(continuationReceivedMessage("x", "y"), &fieldmaskpb.FieldMask{Paths: []string{path}}, appendOnly))
		values := continuationRoot(t, a).GetMessages()[0].GetMessagesReceivedFromAgents().GetMessages()
		want := []string{"x", "y"}
		if appendOnly {
			want = []string{"ax", "by"}
		}
		if values[0].GetMessageBody() != want[0] || values[1].GetMessageBody() != want[1] {
			t.Fatal("repeated message mask did not traverse pairwise")
		}
		before := proto.Clone(a.root)
		action := continuationUpdate(continuationReceivedMessage("z"), &fieldmaskpb.FieldMask{Paths: []string{path}}, appendOnly)
		requireContinuationError(t, a.apply(action), "invalid_mask", int(action.WhichAction()))
		if !proto.Equal(before, a.root) {
			t.Fatal("mismatched array mask partially applied")
		}
	}
}

func TestContinuationSizeLimitsAndLatchedErrors(t *testing.T) {
	if continuationStateLimit != 2<<20 {
		t.Fatal("continuation state limit must be 2 MiB")
	}
	a := newContinuationAccumulator(nil)
	continuationApply(t, a, continuationCreate(continuationText("out", strings.Repeat("x", 200<<10))))
	raw, err := a.snapshot()
	if err != nil || len(raw) <= 128<<10 {
		t.Fatalf("200 KiB state rejected: %v", err)
	}
	if _, err := decodeContinuationState(raw); err != nil {
		t.Fatal(err)
	}
	// The serialized context, not just a string leaf, owns the exact boundary.
	boundary := newContinuationAccumulator(nil)
	boundary.root = warpapi.Task_builder{Id: proto.String("root"), ServerData: proto.String(strings.Repeat("x", continuationStateLimit-32))}.Build()
	context := warpapi.Request_TaskContext_builder{Tasks: []*warpapi.Task{boundary.root}}.Build()
	boundary.root.SetServerData(boundary.root.GetServerData() + strings.Repeat("x", continuationStateLimit-proto.Size(context)))
	atLimit, err := boundary.snapshot()
	if err != nil || len(atLimit) != continuationStateLimit {
		t.Fatalf("exact state size boundary rejected: bytes=%d err=%v", len(atLimit), err)
	}
	if _, err := decodeContinuationState(atLimit); err != nil {
		t.Fatal(err)
	}
	boundary.root.SetServerData(boundary.root.GetServerData() + "x")
	_, err = boundary.snapshot()
	requireContinuationError(t, err, "state_size", 0)
	// Two individually legal actions exceed the aggregate context bound.
	continuationApply(t, a, continuationUpdate(continuationText("out", strings.Repeat("y", 1<<20)), &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}, true))
	action := continuationUpdate(continuationText("out", strings.Repeat("z", 1<<20)), &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}, true)
	first := a.apply(action)
	stateErr := requireContinuationError(t, first, "state_size", int(action.WhichAction()))
	if stateErr.MeasuredBytes <= continuationStateLimit {
		t.Fatal("state size error omitted the measured bound")
	}
	rollback := warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build()
	if a.apply(rollback) != first {
		t.Fatal("later action cleared or replaced the first error")
	}
	if _, err := a.snapshot(); err != first {
		t.Fatal("snapshot did not retain the first error")
	}
	a = newContinuationAccumulator(nil)
	oversized := continuationCreate(continuationText("out", strings.Repeat("x", continuationStateLimit)))
	sizeErr := requireContinuationError(t, a.apply(oversized), "action_size", int(oversized.WhichAction()))
	if sizeErr.MeasuredBytes != proto.Size(oversized) {
		t.Fatal("action error reported an incorrect size")
	}
	_, err = decodeContinuationState(bytes.Repeat([]byte("x"), continuationStateLimit+1))
	requireContinuationError(t, err, "state_size", 0)
}

func TestContinuationErrorReasonsAndContentFreeLabels(t *testing.T) {
	tests := []struct {
		name, reason string
		action       *warpapi.ClientAction
	}{
		{"missing root", "missing_root", continuationUpdate(continuationText("out", "content"), &fieldmaskpb.FieldMask{}, false)},
		{"transaction", "transaction", warpapi.ClientAction_builder{CommitTransaction: warpapi.ClientAction_CommitTransaction_builder{}.Build()}.Build()},
		{"invalid action", "invalid_action", nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := newContinuationAccumulator(nil)
			kind := 0
			if test.action != nil {
				kind = int(test.action.WhichAction())
			}
			requireContinuationError(t, a.apply(test.action), test.reason, kind)
		})
	}
	_, err := newContinuationAccumulator(nil).snapshot()
	requireContinuationError(t, err, "missing_root", 0)
	_, err = decodeContinuationState(nil)
	requireContinuationError(t, err, "missing_root", 0)
	err = continuationError("untrusted-private-content", 0, 0)
	requireContinuationError(t, err, "invalid_action", 0)
	if strings.Contains(err.Error(), "untrusted-private-content") {
		t.Fatal("dynamic reason reached error output")
	}
}

func TestContinuationTransactionFailureCannotBeRolledBackToSuccess(t *testing.T) {
	a := newContinuationAccumulator(nil)
	continuationApply(t, a, continuationCreate(continuationText("out", "before")))
	continuationApply(t, a, warpapi.ClientAction_builder{BeginTransaction: warpapi.ClientAction_BeginTransaction_builder{}.Build()}.Build())
	action := continuationUpdate(continuationText("out", "private"), &fieldmaskpb.FieldMask{Paths: []string{"id"}}, false)
	first := a.apply(action)
	requireContinuationError(t, first, "identity_mutation", int(action.WhichAction()))
	if err := a.apply(warpapi.ClientAction_builder{RollbackTransaction: warpapi.ClientAction_RollbackTransaction_builder{}.Build()}.Build()); err != first {
		t.Fatal("rollback swallowed a latched transaction error")
	}
	if _, err := a.snapshot(); err != first {
		t.Fatal("failed transaction exported a snapshot")
	}
}

func TestLocalBindingWrappersNeverUseRedis(t *testing.T) {
	backend := &continuationMemoryRedis{values: map[string][]byte{}, ttls: map[string]time.Duration{}}
	b := newBindingStore()
	b.redis = backend
	b.save(context.Background(), "call", callBinding{ConversationID: "local-conversation"})
	if _, ok := b.lookup(context.Background(), []string{"call"}); !ok {
		t.Fatal("local wrapper failed")
	}
	if len(backend.values) != 0 {
		t.Fatal("local test state reached Redis")
	}
	fresh := newBindingStore()
	fresh.redis = backend
	if _, ok := fresh.lookup(context.Background(), []string{"call"}); ok {
		t.Fatal("local test state escaped constructor")
	}
}
