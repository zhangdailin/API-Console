package warp

import (
	"regexp"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

// Matches the grouped MCPContext conversion in official convert_to.rs. Local
// client functions are an ad-hoc server: do not fabricate managed identity.
func TestMCPGroupedContextMatchesOfficialConversion(t *testing.T) {
	for _, schema := range []map[string]any{{}, {"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}} {
		raw, err := buildRequest("model-a", preparedRequest{Query: "hello", Tools: []mcpTool{{Name: "acceptance_echo", Description: "Pure echo", Schema: schema}}})
		if err != nil {
			t.Fatal(err)
		}
		var req warpapi.Request
		if proto.Unmarshal(raw, &req) != nil {
			t.Fatal("invalid protobuf")
		}
		mcp := req.GetMcpContext()
		if len(mcp.GetTools()) != 0 || len(mcp.GetResources()) != 0 || len(mcp.GetServers()) != 1 {
			t.Fatal("grouped and flat MCP forms mixed")
		}
		server := mcp.GetServers()[0]
		if server.GetId() != clientMCPServerID || !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(server.GetId()) || server.GetName() != "client" || server.HasIdentity() {
			t.Fatal("ad-hoc server identity changed")
		}
		tools := server.GetTools()
		if len(tools) != 1 || tools[0].GetName() != "acceptance_echo" || tools[0].GetDescription() != "Pure echo" || !tools[0].HasInputSchema() {
			t.Fatal("tool schema or name dropped")
		}
		if len(schema) == 0 && len(tools[0].GetInputSchema().GetFields()) != 0 {
			t.Fatal("empty object schema mutated")
		}
		supported := req.GetSettings().GetSupportedTools()
		if len(supported) != 1 || supported[0] != warpapi.ToolType_CALL_MCP_TOOL {
			t.Fatal("unexpected native tool capability")
		}
	}
}
