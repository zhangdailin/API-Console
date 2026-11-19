package warp

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"

	"orchids-api/internal/debug"
)

func TestMCPDiagnosticsDistinguishDeclarationsAndCallsWithoutContent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tools     int
		choice    any
		invoke    bool
		wantTools int
		wantCalls int
	}{
		{name: "no tools"},
		{name: "auto unused", tools: 111, wantTools: 111},
		{name: "none", tools: 111, choice: "none"},
		{name: "called", tools: 1, invoke: true, wantTools: 1, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const private = "private-mcp-content"
			req := testRequest()
			req.ToolChoice = tc.choice
			for i := 0; i < tc.tools; i++ {
				req.Tools = append(req.Tools, map[string]any{
					"name": fmt.Sprintf("%s-%d", private, i), "description": private,
					"input_schema": map[string]any{"type": "object", "description": private},
				})
			}
			stream := integrityStream(t, testMessages(testOutput("private-message", private)), testDone())
			if tc.invoke {
				call := integrityMCP(t, "private-message", "private-call", private+"-0", map[string]any{"text": private}, proto.String(clientMCPServerID))
				stream = integrityStream(t, initEvent(t, "private-conversation"), testAction(continuationCreate(call)), testDone())
			}
			account := testAccount()
			account.ID = 7
			client := NewFromAccount(account, testConfig("https://private.invalid"))
			calls := 0
			client.http = &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				var wire warpapi.Request
				if err != nil || proto.Unmarshal(body, &wire) != nil {
					t.Fatal("invalid outgoing request")
				}
				count := 0
				for _, server := range wire.GetMcpContext().GetServers() {
					count += len(server.GetTools())
					if server.GetId() != clientMCPServerID {
						t.Fatal("outgoing installation was not the expected UUID")
					}
				}
				if count != tc.wantTools {
					t.Fatal("wire declarations do not match the diagnostic count")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(stream)), Request: r}, nil
			})}
			ctx, capture := debug.WithCapture(bindingAuthContext(t, 1, "synthetic-diagnostic-key"), "test-request")
			if err := client.SendRequestWithPayload(ctx, req, nil, nil); err != nil {
				t.Fatal(err)
			}
			sections := capture.Bundle().Sections
			if calls != 1 || len(sections) != 1 || sections[0].Name != "3_warp_diagnostic.json" {
				t.Fatal("unexpected retry or capture section")
			}
			var diagnostic generationDiagnostic
			if json.Unmarshal([]byte(sections[0].Payload), &diagnostic) != nil || diagnostic.Stage != "success" || diagnostic.MCPToolsDeclared != tc.wantTools || diagnostic.MCPToolCalls != tc.wantCalls {
				t.Fatalf("wrong content-free MCP diagnostic: %s", sections[0].Payload)
			}
			for _, forbidden := range []string{private, "private-message", "private-call", "private-conversation", clientMCPServerID, "synthetic-diagnostic-key"} {
				if strings.Contains(sections[0].Payload, forbidden) {
					t.Fatal("MCP diagnostic exposed request or response content")
				}
			}
		})
	}
}
