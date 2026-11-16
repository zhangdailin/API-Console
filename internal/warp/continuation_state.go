package warp

import (
	"errors"
	"strings"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const continuationStateLimit = 128 << 10

var errContinuationState = errors.New("invalid warp continuation state")

// continuationAccumulator reconstructs the official task context, not a text
// transcript. It receives only actions already accepted by the stream parser.
// All protobuf inputs and exported bytes are copied; no opaque content is logged.
type continuationAccumulator struct {
	root          *warpapi.Task
	transaction   *warpapi.Task
	inTransaction bool
	err           error
}

func newContinuationAccumulator(initial []byte) *continuationAccumulator {
	a := &continuationAccumulator{}
	if len(initial) > 0 {
		a.root, a.err = decodeContinuationState(initial)
	}
	return a
}

func decodeContinuationState(raw []byte) (*warpapi.Task, error) {
	if len(raw) == 0 || len(raw) > continuationStateLimit {
		return nil, errContinuationState
	}
	context := &warpapi.Request_TaskContext{}
	if proto.Unmarshal(raw, context) != nil || hasUnknown(context.ProtoReflect()) || len(context.GetTasks()) != 1 {
		return nil, errContinuationState
	}
	root := context.GetTasks()[0]
	if !validContinuationTask(root) {
		return nil, errContinuationState
	}
	return root, nil
}

func validContinuationTask(root *warpapi.Task) bool {
	if root == nil || root.GetId() == "" || root.GetDependencies().GetParentTaskId() != "" || hasUnknown(root.ProtoReflect()) {
		return false
	}
	seen := make(map[string]bool, len(root.GetMessages()))
	for _, message := range root.GetMessages() {
		if message == nil || message.GetId() == "" || seen[message.GetId()] || (message.GetTaskId() != "" && message.GetTaskId() != root.GetId()) {
			return false
		}
		seen[message.GetId()] = true
	}
	return true
}

func (a *continuationAccumulator) apply(action *warpapi.ClientAction) error {
	if a == nil {
		return errContinuationState
	}
	if a.err != nil {
		return a.err
	}
	a.err = a.applyAction(action)
	return a.err
}

func (a *continuationAccumulator) applyAction(action *warpapi.ClientAction) error {
	if action == nil || hasUnknown(action.ProtoReflect()) || proto.Size(action) > continuationStateLimit {
		return errContinuationState
	}
	switch action.WhichAction() {
	case warpapi.ClientAction_BeginTransaction_case:
		if a.inTransaction {
			return errContinuationState
		}
		a.inTransaction = true
		if a.root != nil {
			a.transaction = proto.Clone(a.root).(*warpapi.Task)
		}
	case warpapi.ClientAction_CommitTransaction_case:
		if !a.inTransaction {
			return errContinuationState
		}
		a.transaction, a.inTransaction = nil, false
	case warpapi.ClientAction_RollbackTransaction_case:
		if !a.inTransaction {
			return errContinuationState
		}
		a.root, a.transaction, a.inTransaction = a.transaction, nil, false
	case warpapi.ClientAction_CreateTask_case:
		root := action.GetCreateTask().GetTask()
		if a.root != nil || !validContinuationTask(root) {
			return errContinuationState
		}
		a.root = proto.Clone(root).(*warpapi.Task)
	case warpapi.ClientAction_AddMessagesToTask_case:
		add := action.GetAddMessagesToTask()
		if !a.matchesTask(add.GetTaskId()) {
			return errContinuationState
		}
		messages := append([]*warpapi.Message(nil), a.root.GetMessages()...)
		for _, message := range add.GetMessages() {
			if message == nil {
				return errContinuationState
			}
			messages = append(messages, proto.Clone(message).(*warpapi.Message))
		}
		a.root.SetMessages(messages)
	case warpapi.ClientAction_UpdateTaskMessage_case:
		update := action.GetUpdateTaskMessage()
		if !a.matchesTask(update.GetTaskId()) {
			return errContinuationState
		}
		if err := a.updateMessage(update.GetMessage(), update.GetMask(), false); err != nil {
			return err
		}
	case warpapi.ClientAction_AppendToMessageContent_case:
		appendAction := action.GetAppendToMessageContent()
		if !a.matchesTask(appendAction.GetTaskId()) {
			return errContinuationState
		}
		if err := a.updateMessage(appendAction.GetMessage(), appendAction.GetMask(), true); err != nil {
			return err
		}
	case warpapi.ClientAction_UpdateTaskServerData_case:
		update := action.GetUpdateTaskServerData()
		if !a.matchesTask(update.GetTaskId()) {
			return errContinuationState
		}
		a.root.SetServerData(update.GetServerData())
	case warpapi.ClientAction_UpdateTaskDescription_case:
		update := action.GetUpdateTaskDescription()
		if !a.matchesTask(update.GetTaskId()) {
			return errContinuationState
		}
		a.root.SetDescription(update.GetDescription())
	case warpapi.ClientAction_UpdateTaskSummary_case:
		update := action.GetUpdateTaskSummary()
		if !a.matchesTask(update.GetTaskId()) {
			return errContinuationState
		}
		a.root.SetSummary(update.GetSummary())
	case warpapi.ClientAction_ShowSuggestions_case:
		// Suggestions are not part of Request.TaskContext.
	default:
		return errContinuationState
	}
	if a.root != nil {
		context := warpapi.Request_TaskContext_builder{Tasks: []*warpapi.Task{a.root}}.Build()
		if !validContinuationTask(a.root) || proto.Size(context) > continuationStateLimit {
			return errContinuationState
		}
	}
	return nil
}

func (a *continuationAccumulator) matchesTask(id string) bool {
	return a.root != nil && id == a.root.GetId()
}

func (a *continuationAccumulator) updateMessage(message *warpapi.Message, mask *fieldmaskpb.FieldMask, appendOnly bool) error {
	if message == nil || message.GetId() == "" || (message.GetTaskId() != "" && message.GetTaskId() != a.root.GetId()) {
		return errContinuationState
	}
	messages := a.root.GetMessages()
	index := -1
	for i, existing := range messages {
		if existing.GetId() == message.GetId() {
			index = i
			break
		}
	}
	if index < 0 {
		return errContinuationState
	}
	if len(mask.GetPaths()) == 0 {
		if !appendOnly {
			// An unmasked update is a complete replacement, never proto.Merge:
			// Merge appends repeated fields and retains stale oneof content.
			messages[index] = proto.Clone(message).(*warpapi.Message)
			a.root.SetMessages(messages)
			return nil
		}
		// Legacy text deltas omit a mask. Only AgentOutput.text can be
		// interpreted unambiguously without an explicit append mask.
		if message.WhichMessage() != warpapi.Message_AgentOutput_case || messages[index].WhichMessage() != warpapi.Message_AgentOutput_case {
			return errContinuationState
		}
		mask = &fieldmaskpb.FieldMask{Paths: []string{"agent_output.text"}}
	}
	if !mask.IsValid(message) {
		return errContinuationState
	}
	// A field mask is a set. Normalize a clone so duplicate append paths cannot
	// duplicate content and the caller's event remains unchanged.
	mask = proto.Clone(mask).(*fieldmaskpb.FieldMask)
	mask.Normalize()
	destination := proto.Clone(messages[index]).(*warpapi.Message)
	for _, path := range mask.GetPaths() {
		if path == "id" || path == "task_id" || strings.HasPrefix(path, "id.") || strings.HasPrefix(path, "task_id.") {
			return errContinuationState
		}
		if err := applyContinuationField(destination.ProtoReflect(), message.ProtoReflect(), strings.Split(path, "."), appendOnly); err != nil {
			return err
		}
	}
	messages[index] = destination
	a.root.SetMessages(messages)
	return nil
}

// applyContinuationField uses protobuf field descriptors so masked nested tool
// updates preserve untouched fields. Replacement clears collections first;
// appending is permitted only for explicitly named string leaves.
func applyContinuationField(destination, source protoreflect.Message, path []string, appendOnly bool) error {
	field := source.Descriptor().Fields().ByName(protoreflect.Name(path[0]))
	if field == nil {
		return errContinuationState
	}
	if len(path) > 1 {
		if field.Kind() != protoreflect.MessageKind || field.IsList() || field.IsMap() || !source.Has(field) {
			return errContinuationState
		}
		return applyContinuationField(destination.Mutable(field).Message(), source.Get(field).Message(), path[1:], appendOnly)
	}
	if appendOnly {
		if field.Kind() != protoreflect.StringKind || field.IsList() || field.IsMap() {
			return errContinuationState
		}
		destination.Set(field, protoreflect.ValueOfString(destination.Get(field).String()+source.Get(field).String()))
		return nil
	}
	// Clone just this field through a temporary message to copy lists, maps,
	// bytes and nested messages without retaining aliases to the stream event.
	temporary := source.New()
	if source.Has(field) {
		temporary.Set(field, source.Get(field))
	}
	temporary = proto.Clone(temporary.Interface()).ProtoReflect()
	destination.Clear(field)
	if temporary.Has(field) {
		destination.Set(field, temporary.Get(field))
	}
	return nil
}

// snapshot returns independently owned official Request.TaskContext bytes.
func (a *continuationAccumulator) snapshot() ([]byte, error) {
	if a == nil || a.err != nil || a.inTransaction || !validContinuationTask(a.root) {
		return nil, errContinuationState
	}
	context := warpapi.Request_TaskContext_builder{Tasks: []*warpapi.Task{a.root}}.Build()
	if proto.Size(context) > continuationStateLimit {
		return nil, errContinuationState
	}
	return proto.MarshalOptions{Deterministic: true}.Marshal(context)
}
