package warp

import (
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// A stable virtual installation UUID, not a managed Warp server identity.
	// The official client uses installation UUIDs for grouped MCP routing.
	clientMCPServerID = "d8db81de-9599-531a-9f9f-9053f843a477"
	legacyMCPServerID = "client-request-tools"
)

// mcpServerIDForRoot preserves the installation of an already-owned tool
// conversation across upgrades. Only private, validated continuation state may
// select the legacy ID; a new upstream response cannot enable legacy routing.
func mcpServerIDForRoot(root *warpapi.Task) (string, error) {
	selected := ""
	for _, message := range root.GetMessages() {
		call := message.GetToolCall().GetCallMcpTool()
		if call == nil || !call.HasServerId() {
			continue
		}
		id := call.GetServerId()
		if (id != clientMCPServerID && id != legacyMCPServerID) || (selected != "" && selected != id) {
			return "", errContinuationState
		}
		selected = id
	}
	if selected == "" {
		selected = clientMCPServerID
	}
	return selected, nil
}

// buildMCPContext declares only tools explicitly supplied by the client. Their
// installation UUID does not enable native tools or imply managed identity.
func buildMCPContext(tools []mcpTool, serverID string) *warpapi.Request_MCPContext {
	if len(tools) == 0 {
		return nil
	}
	declared := make([]*warpapi.Request_MCPContext_MCPTool, 0, len(tools))
	for _, tool := range tools {
		builder := warpapi.Request_MCPContext_MCPTool_builder{Name: &tool.Name}
		if tool.Description != "" {
			builder.Description = &tool.Description
		}
		// An empty JSON object is still an explicitly supplied valid schema.
		// The official converter always sets input_schema after conversion.
		if tool.Schema != nil {
			if schema, err := structpb.NewStruct(tool.Schema); err == nil {
				builder.InputSchema = schema
			}
		}
		declared = append(declared, builder.Build())
	}
	server := warpapi.Request_MCPContext_MCPServer_builder{
		Name:        proto.String("client"),
		Description: proto.String("Tools declared by the client request"),
		Id:          proto.String(serverID),
		Tools:       declared,
	}.Build()
	return warpapi.Request_MCPContext_builder{Servers: []*warpapi.Request_MCPContext_MCPServer{server}}.Build()
}
