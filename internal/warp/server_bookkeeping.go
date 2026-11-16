package warp

import warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"

const serverBookkeepingPayloadLimit = 64 << 10

// isServerBookkeeping identifies the two opaque variants that Warp's client
// converter maps to NoClientRepresentation. This means no client action is
// required, not that upstream execution or charges did not occur. Payloads are
// bounded but never interpreted, executed, retained, logged, or exposed as text.
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
