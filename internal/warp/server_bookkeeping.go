package warp

import (
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

const serverBookkeepingPayloadLimit = 64 << 10

// isServerBookkeeping identifies the two opaque variants that Warp's client
// converter maps to NoClientRepresentation. This means no client action is
// required, not that upstream execution or charges did not occur. Payloads are
// bounded but never interpreted, executed, logged, or exposed as text. The
// original official message is retained privately by the continuation accumulator.
//
// Reference: https://github.com/warpdotdev/warp/blob/c6f07bb451657e6fbce72c69ed90cfd4f86c2344/app/src/ai/agent/api/convert_from.rs
func isServerBookkeeping(message *warpapi.Message) bool {
	if message == nil || hasUnknown(message.ProtoReflect()) {
		return false
	}
	switch message.WhichMessage() {
	case warpapi.Message_ToolCall_case:
		call := message.GetToolCall()
		if call == nil || call.WhichTool() != warpapi.Message_ToolCall_Server_case {
			return false
		}
		server := call.GetServer()
		return server != nil && len(server.GetPayload()) <= serverBookkeepingPayloadLimit
	case warpapi.Message_ServerEvent_case:
		event := message.GetServerEvent()
		return event != nil && len(event.GetPayload()) <= serverBookkeepingPayloadLimit
	default:
		return false
	}
}

// observedServerCalls tracks only validated call identities, never payloads.
// Transaction snapshots include the root binding, so a rolled-back CreateTask
// cannot leave behind a pairing capability or prevent the real root creation.
type observedServerCalls struct {
	calls, transaction map[string]bool
	taskID             string
	created            bool
	bookkeeping        int
}

func (s *observedServerCalls) observe(message *warpapi.Message, rootID string) {
	if !isServerBookkeeping(message) || message.GetToolCall() == nil || rootID == "" || (message.GetTaskId() != "" && message.GetTaskId() != rootID) {
		return
	}
	if id := message.GetToolCall().GetToolCallId(); id != "" {
		s.calls[id] = true
	}
}

func (s *observedServerCalls) begin(taskID string, created bool, count int) {
	s.transaction = make(map[string]bool, len(s.calls))
	for id := range s.calls {
		s.transaction[id] = true
	}
	s.taskID, s.created, s.bookkeeping = taskID, created, count
}

func (s *observedServerCalls) rollback() (string, bool, int) {
	s.calls, s.transaction = s.transaction, nil
	return s.taskID, s.created, s.bookkeeping
}

// validateServerResultAction prevents an opaque result from turning an invalid
// task-state action into a successful text-only completion. Keep the historical
// leniency for unrelated text fixtures, but fail closed for this new variant.
func validateServerResultAction(state *continuationAccumulator, action *warpapi.ClientAction) error {
	if state == nil || state.err != nil {
		return errStreamStream
	}
	candidate := &continuationAccumulator{}
	if state.root != nil {
		candidate.root = proto.Clone(state.root).(*warpapi.Task)
	}
	if candidate.apply(action) != nil {
		return errStreamStream
	}
	return nil
}

// effectiveBookkeepingMessage validates a masked update against the original
// task state before classifying it. Checking only its delta could bypass the
// byte bound (append), lose the paired call ID, or retain unreviewed mask paths.
func effectiveBookkeepingMessage(state *continuationAccumulator, action *warpapi.ClientAction, message *warpapi.Message) (*warpapi.Message, error) {
	var update, masked bool
	switch action.WhichAction() {
	case warpapi.ClientAction_UpdateTaskMessage_case:
		update, masked = true, len(action.GetUpdateTaskMessage().GetMask().GetPaths()) > 0
	case warpapi.ClientAction_AppendToMessageContent_case:
		update, masked = true, len(action.GetAppendToMessageContent().GetMask().GetPaths()) > 0
	}
	// A mask must never hide a prohibited native variant in the supplied delta.
	if call := message.GetToolCall(); call != nil && call.WhichTool() != warpapi.Message_ToolCall_Server_case && call.WhichTool() != warpapi.Message_ToolCall_CallMcpTool_case {
		return message, nil
	}
	if result := message.GetToolCallResult(); result != nil && result.WhichResult() != warpapi.Message_ToolCallResult_Server_case && result.WhichResult() != warpapi.Message_ToolCallResult_CallMcpTool_case {
		return message, nil
	}
	isOpaque := func(m *warpapi.Message) bool {
		return m.GetToolCallResult() != nil || m.GetServerEvent() != nil || (m.GetToolCall() != nil && m.GetToolCall().GetServer() != nil)
	}
	opaque := isOpaque(message)
	if update && state != nil && state.root != nil {
		for _, original := range state.root.GetMessages() {
			if original.GetId() == message.GetId() && isOpaque(original) {
				opaque = true
				break
			}
		}
	}
	if !opaque || !update {
		return message, nil
	}
	// Bound supplied opaque data even when its mask would not select it.
	if message.GetToolCall() != nil || message.GetServerEvent() != nil {
		if !isServerBookkeeping(message) {
			return message, nil
		}
	}
	if result := message.GetToolCallResult(); result != nil && result.GetServer() != nil && len(result.GetServer().GetSerializedResult()) > serverBookkeepingPayloadLimit {
		return message, nil
	}
	if !masked && action.WhichAction() == warpapi.ClientAction_AppendToMessageContent_case {
		return nil, errStreamStream
	}
	if state == nil || state.err != nil || state.root == nil {
		return nil, errStreamStream
	}
	candidate := &continuationAccumulator{root: proto.Clone(state.root).(*warpapi.Task)}
	if candidate.apply(action) != nil {
		return nil, errStreamStream
	}
	for _, effective := range candidate.root.GetMessages() {
		if effective.GetId() == message.GetId() {
			return effective, nil
		}
	}
	return nil, errStreamStream
}
