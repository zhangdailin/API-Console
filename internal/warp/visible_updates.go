package warp

import (
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

// effectiveVisibleUpdate projects the same masked text snapshot as task history.
// Once a real root exists, a delta must not be exposed as unmasked output.
func effectiveVisibleUpdate(state *continuationAccumulator, action *warpapi.ClientAction, message *warpapi.Message) (*warpapi.Message, bool, error) {
	update := action.WhichAction() == warpapi.ClientAction_UpdateTaskMessage_case || action.WhichAction() == warpapi.ClientAction_AppendToMessageContent_case
	if !update || state == nil || state.root == nil {
		return message, false, nil
	}
	if state.err != nil {
		return nil, false, state.err
	}
	// A mask is not permission to hide a native or unknown executable variant.
	if call := message.GetToolCall(); call != nil && call.WhichTool() != warpapi.Message_ToolCall_Server_case && call.WhichTool() != warpapi.Message_ToolCall_CallMcpTool_case {
		return message, false, nil
	}
	if result := message.GetToolCallResult(); result != nil && result.WhichResult() != warpapi.Message_ToolCallResult_Server_case && result.WhichResult() != warpapi.Message_ToolCallResult_CallMcpTool_case {
		return message, false, nil
	}
	candidate := &continuationAccumulator{root: proto.Clone(state.root).(*warpapi.Task)}
	if err := candidate.apply(action); err != nil {
		return nil, false, err
	}
	for _, effective := range candidate.root.GetMessages() {
		if effective.GetId() == message.GetId() {
			return effective, true, nil
		}
	}
	return nil, false, errContinuationState
}
