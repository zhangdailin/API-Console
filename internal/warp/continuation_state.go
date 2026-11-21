package warp

import (
	"errors"
	"fmt"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// Continuation state stays bounded below the stream's 8 MiB frame ceiling.
const continuationStateLimit = 2 << 20

var errContinuationState = errors.New("invalid warp continuation state")

// continuationStateError contains only fixed labels, action numbers and sizes,
// never task identifiers, mask paths, protobuf content or underlying errors.
type continuationStateError struct {
	Reason                    string
	ActionKind                int
	MeasuredBytes, LimitBytes int
}

func (e *continuationStateError) Error() string {
	return fmt.Sprintf("invalid warp continuation state: reason=%s action=%d measured_bytes=%d limit_bytes=%d", e.Reason, e.ActionKind, e.MeasuredBytes, e.LimitBytes)
}

func (e *continuationStateError) Unwrap() error { return errContinuationState }

func continuationError(reason string, actionKind, measuredBytes int) error {
	// Keep labels bounded even if a future caller passes dynamic input.
	switch reason {
	case "action_size", "state_size", "missing_root", "missing_message", "invalid_task", "invalid_mask", "identity_mutation", "transaction", "unknown_fields", "invalid_action", "unsupported_action":
	default:
		reason = "invalid_action"
	}
	return &continuationStateError{Reason: reason, ActionKind: actionKind, MeasuredBytes: measuredBytes, LimitBytes: continuationStateLimit}
}

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
	taskContext, err := decodeTaskContext(raw)
	if err != nil {
		return nil, err
	}
	return taskContext.GetTasks()[0], nil
}

func decodeTaskContext(raw []byte) (*warpapi.Request_TaskContext, error) {
	if len(raw) > continuationStateLimit {
		return nil, continuationError("state_size", 0, len(raw))
	}
	context := &warpapi.Request_TaskContext{}
	if proto.Unmarshal(raw, context) != nil {
		return nil, continuationError("invalid_task", 0, 0)
	}
	if hasUnknown(context.ProtoReflect()) {
		return nil, continuationError("unknown_fields", 0, 0)
	}
	if len(context.GetTasks()) == 0 {
		return nil, continuationError("missing_root", 0, 0)
	}
	if len(context.GetTasks()) != 1 || !validContinuationTask(context.GetTasks()[0]) {
		return nil, continuationError("invalid_task", 0, 0)
	}
	return context, nil
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
		return continuationError("missing_root", 0, 0)
	}
	if a.err != nil {
		return a.err
	}
	a.err = a.applyAction(action)
	return a.err
}

func (a *continuationAccumulator) applyAction(action *warpapi.ClientAction) error {
	if action == nil {
		return continuationError("invalid_action", 0, 0)
	}
	kind := int(action.WhichAction())
	fail := func(reason string) error { return continuationError(reason, kind, 0) }
	if size := proto.Size(action); size > continuationStateLimit {
		return continuationError("action_size", kind, size)
	}
	if hasUnknown(action.ProtoReflect()) {
		return fail("unknown_fields")
	}
	checkTask := func(id string) error {
		if a.root == nil {
			return fail("missing_root")
		}
		if !a.matchesTask(id) {
			return fail("invalid_task")
		}
		return nil
	}
	switch action.WhichAction() {
	case warpapi.ClientAction_BeginTransaction_case:
		if a.inTransaction {
			return fail("transaction")
		}
		a.inTransaction = true
		if a.root != nil {
			a.transaction = proto.Clone(a.root).(*warpapi.Task)
		}
	case warpapi.ClientAction_CommitTransaction_case:
		if !a.inTransaction {
			return fail("transaction")
		}
		a.transaction, a.inTransaction = nil, false
	case warpapi.ClientAction_RollbackTransaction_case:
		if !a.inTransaction {
			return fail("transaction")
		}
		a.root, a.transaction, a.inTransaction = a.transaction, nil, false
	case warpapi.ClientAction_CreateTask_case:
		root := action.GetCreateTask().GetTask()
		if a.root != nil || !validContinuationTask(root) {
			return fail("invalid_task")
		}
		a.root = proto.Clone(root).(*warpapi.Task)
	case warpapi.ClientAction_AddMessagesToTask_case:
		add := action.GetAddMessagesToTask()
		if err := checkTask(add.GetTaskId()); err != nil {
			return err
		}
		messages := append([]*warpapi.Message(nil), a.root.GetMessages()...)
		for _, message := range add.GetMessages() {
			if message == nil {
				return fail("missing_message")
			}
			messages = append(messages, proto.Clone(message).(*warpapi.Message))
		}
		a.root.SetMessages(messages)
	case warpapi.ClientAction_UpdateTaskMessage_case:
		update := action.GetUpdateTaskMessage()
		if err := checkTask(update.GetTaskId()); err != nil {
			return err
		}
		if err := a.updateMessage(update.GetMessage(), update.GetMask(), false, kind); err != nil {
			return err
		}
	case warpapi.ClientAction_AppendToMessageContent_case:
		appendAction := action.GetAppendToMessageContent()
		if err := checkTask(appendAction.GetTaskId()); err != nil {
			return err
		}
		if err := a.updateMessage(appendAction.GetMessage(), appendAction.GetMask(), true, kind); err != nil {
			return err
		}
	case warpapi.ClientAction_UpdateTaskServerData_case:
		update := action.GetUpdateTaskServerData()
		if err := checkTask(update.GetTaskId()); err != nil {
			return err
		}
		a.root.SetServerData(update.GetServerData())
	case warpapi.ClientAction_UpdateTaskDescription_case:
		update := action.GetUpdateTaskDescription()
		if err := checkTask(update.GetTaskId()); err != nil {
			return err
		}
		a.root.SetDescription(update.GetDescription())
	case warpapi.ClientAction_UpdateTaskSummary_case:
		update := action.GetUpdateTaskSummary()
		if err := checkTask(update.GetTaskId()); err != nil {
			return err
		}
		a.root.SetSummary(update.GetSummary())
	case warpapi.ClientAction_ShowSuggestions_case:
		// Suggestions are not part of Request.TaskContext.
	default:
		return fail("unsupported_action")
	}
	if a.root != nil {
		if !validContinuationTask(a.root) {
			return fail("invalid_task")
		}
		context := warpapi.Request_TaskContext_builder{Tasks: []*warpapi.Task{a.root}}.Build()
		if size := proto.Size(context); size > continuationStateLimit {
			return continuationError("state_size", kind, size)
		}
	}
	return nil
}

func (a *continuationAccumulator) matchesTask(id string) bool {
	return a.root != nil && id == a.root.GetId()
}

func (a *continuationAccumulator) updateMessage(message *warpapi.Message, mask *fieldmaskpb.FieldMask, appendOnly bool, kind int) error {
	// The official client skips actions without a mask. A present empty mask
	// differs: it can upsert a new message, but changes no existing fields.
	if mask == nil {
		return nil
	}
	if message == nil || message.GetId() == "" {
		return continuationError("missing_message", kind, 0)
	}
	if message.GetTaskId() != "" && message.GetTaskId() != a.root.GetId() {
		return continuationError("invalid_task", kind, 0)
	}
	for _, path := range mask.GetPaths() {
		if reason := validateContinuationPath(message.ProtoReflect().Descriptor(), path, appendOnly); reason != "" {
			return continuationError(reason, kind, 0)
		}
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
		if appendOnly {
			return continuationError("missing_message", kind, 0)
		}
		// Official UpdateTaskMessage inserts the full supplied message, not a
		// masked projection. applyAction validates unknown fields and the full
		// resulting task, including identity, ownership and size limits.
		a.root.SetMessages(append(append([]*warpapi.Message(nil), messages...), proto.Clone(message).(*warpapi.Message)))
		return nil
	}
	destination := proto.Clone(messages[index]).(*warpapi.Message)
	// Paths are ordered operations, not a set: preserve duplicates and parent/
	// child order (especially repeated string appends). Never normalize them.
	for _, path := range mask.GetPaths() {
		if err := applyContinuationField(destination.ProtoReflect(), message.ProtoReflect(), splitContinuationPath(path), appendOnly); err != nil {
			var stateErr *continuationStateError
			if errors.As(err, &stateErr) {
				return continuationError(stateErr.Reason, kind, stateErr.MeasuredBytes)
			}
			return continuationError("invalid_mask", kind, 0)
		}
		if size := proto.Size(destination); size > continuationStateLimit {
			return continuationError("state_size", kind, size)
		}
	}
	messages = append([]*warpapi.Message(nil), messages...)
	messages[index] = destination
	a.root.SetMessages(messages)
	return nil
}

// snapshot returns independently owned official Request.TaskContext bytes.
func (a *continuationAccumulator) snapshot() ([]byte, error) {
	if a == nil {
		return nil, continuationError("missing_root", 0, 0)
	}
	if a.err != nil {
		return nil, a.err
	}
	if a.inTransaction {
		return nil, continuationError("transaction", 0, 0)
	}
	if a.root == nil {
		return nil, continuationError("missing_root", 0, 0)
	}
	if !validContinuationTask(a.root) {
		return nil, continuationError("invalid_task", 0, 0)
	}
	context := warpapi.Request_TaskContext_builder{Tasks: []*warpapi.Task{a.root}}.Build()
	if size := proto.Size(context); size > continuationStateLimit {
		return nil, continuationError("state_size", 0, size)
	}
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(context)
	if err != nil {
		return nil, continuationError("invalid_task", 0, 0)
	}
	return raw, nil
}
