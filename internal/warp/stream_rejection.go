package warp

import warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"

// streamRejection contains only fixed classifications, ordinals, enum numbers,
// sizes and identity-presence/equality flags. It must never carry protocol IDs,
// arguments, opaque server data, result content or upstream error text.
type streamRejection struct {
	Reason, Site                                     string
	ActionKind, MessageKind, ResultKind              int
	EventOrdinal, ActionOrdinal, MessageOrdinal      int
	HasRootTaskID, HasActionTaskID, HasMessageTaskID bool
	HasCallID                                        bool
	ActionTaskMatchesRoot, MessageTaskMatchesRoot    bool
	Correlation                                      string
	PayloadBytes                                     int
	HasUnknown                                       bool
}

func rejectionAt(reason, site, correlation, rootID string, event, actionOrdinal, messageOrdinal int, action *warpapi.ClientAction, message *warpapi.Message) *streamRejection {
	actionID := actionTaskID(action)
	r := &streamRejection{
		Reason: reason, Site: site, Correlation: correlation,
		ActionKind: int(action.WhichAction()), MessageKind: int(message.WhichMessage()),
		EventOrdinal: event, ActionOrdinal: actionOrdinal, MessageOrdinal: messageOrdinal,
		HasRootTaskID: rootID != "", HasActionTaskID: actionID != "", HasMessageTaskID: message.GetTaskId() != "",
		ActionTaskMatchesRoot:  rootID != "" && actionID == rootID,
		MessageTaskMatchesRoot: rootID != "" && message.GetTaskId() == rootID,
	}
	if result := message.GetToolCallResult(); result != nil {
		r.ResultKind = int(result.WhichResult())
		r.HasCallID = result.GetToolCallId() != ""
		if result.GetServer() != nil {
			r.PayloadBytes = len(result.GetServer().GetSerializedResult())
		}
	} else if call := message.GetToolCall(); call != nil {
		r.HasCallID = call.GetToolCallId() != ""
	}
	return r
}

func actionTaskID(action *warpapi.ClientAction) string {
	switch action.WhichAction() {
	case warpapi.ClientAction_CreateTask_case:
		return action.GetCreateTask().GetTask().GetId()
	case warpapi.ClientAction_AddMessagesToTask_case:
		return action.GetAddMessagesToTask().GetTaskId()
	case warpapi.ClientAction_UpdateTaskMessage_case:
		return action.GetUpdateTaskMessage().GetTaskId()
	case warpapi.ClientAction_AppendToMessageContent_case:
		return action.GetAppendToMessageContent().GetTaskId()
	}
	return ""
}

func rejectToolResult(result *streamResult, rootID string, actionOrdinal, messageOrdinal int, action *warpapi.ClientAction, message *warpapi.Message, previousMCP, serverCalls map[string]bool) error {
	value := message.GetToolCallResult()
	reason, correlation := "unsupported_result", "unsupported_result"
	err := errStreamTool
	switch value.WhichResult() {
	case warpapi.Message_ToolCallResult_CallMcpTool_case:
		if value.GetToolCallId() != "" && previousMCP[value.GetToolCallId()] {
			return nil
		}
		reason, correlation = "unbound_mcp", "unbound_mcp"
	case warpapi.Message_ToolCallResult_Server_case:
		reason, correlation = "server_unpaired", "unpaired_server"
		if value.GetToolCallId() == "" {
			correlation = "missing_call_id"
		} else if serverCalls[value.GetToolCallId()] {
			correlation = "paired_server"
			if value.GetServer() != nil && len(value.GetServer().GetSerializedResult()) <= serverBookkeepingPayloadLimit {
				result.ServerBookkeeping++
				return nil
			}
		}
		if value.GetServer() != nil && len(value.GetServer().GetSerializedResult()) > serverBookkeepingPayloadLimit {
			reason, err = "server_oversize", errStreamLimit
		}
	}
	result.ToolRequested = true
	result.RejectMessage = int(message.WhichMessage())
	result.Rejection = rejectionAt(reason, "tool_call_result", correlation, rootID, result.Events, actionOrdinal, messageOrdinal, action, message)
	return err
}
