package warp

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
)

func mcpContextSnapshot(t *testing.T, ids ...*string) []byte {
	t.Helper()
	messages := make([]*warpapi.Message, 0, len(ids))
	for i, id := range ids {
		messages = append(messages, integrityMCP(t, strings.Repeat("m", i+1), strings.Repeat("c", i+1), "echo", map[string]any{}, id))
	}
	a := newContinuationAccumulator(nil)
	continuationApply(t, a, continuationCreate(messages...))
	raw, err := a.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMCPInstallationIDPersistsAcrossBoundContinuations(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []*string
		want string
	}{
		{"new", nil, clientMCPServerID},
		{"uuid", []*string{proto.String(clientMCPServerID)}, clientMCPServerID},
		{"legacy", []*string{proto.String(legacyMCPServerID)}, legacyMCPServerID},
		{"unspecified", []*string{nil}, clientMCPServerID},
		{"legacy with unspecified", []*string{nil, proto.String(legacyMCPServerID)}, legacyMCPServerID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var initial []byte
			if tc.ids != nil {
				initial = mcpContextSnapshot(t, tc.ids...)
			}
			before := bytes.Clone(initial)
			prepared := preparedRequest{Query: "hello", Tools: []mcpTool{{Name: "echo", Schema: map[string]any{"type": "object"}}}, RootTaskContext: initial}
			first, err := buildRequest("model-a", prepared)
			if err != nil {
				t.Fatal(err)
			}
			second, err := buildRequest("model-a", prepared)
			if err != nil || !bytes.Equal(first, second) || !bytes.Equal(before, initial) {
				t.Fatal("installation routing changed deterministic output or mutated private history")
			}
			var request warpapi.Request
			if proto.Unmarshal(first, &request) != nil {
				t.Fatal("invalid request")
			}
			servers := request.GetMcpContext().GetServers()
			if len(servers) != 1 || servers[0].GetId() != tc.want || servers[0].HasIdentity() {
				t.Fatal("owned installation changed or fabricated a managed identity")
			}
			if len(initial) > 0 {
				original := &warpapi.Request_TaskContext{}
				if proto.Unmarshal(initial, original) != nil || !proto.Equal(original, request.GetTaskContext()) {
					t.Fatal("installation migration rewrote the original task")
				}
			}
			settings := request.GetSettings()
			if settings.GetWebSearchEnabled() || settings.GetWebContextRetrievalEnabled() || len(settings.GetSupportedTools()) != 1 || settings.GetSupportedTools()[0] != warpapi.ToolType_CALL_MCP_TOOL {
				t.Fatal("MCP routing enabled native or server-side tools")
			}
		})
	}
}

func TestMCPInstallationRejectsForeignOrMixedBoundIDs(t *testing.T) {
	for _, ids := range [][]*string{
		{proto.String("foreign-server")},
		{proto.String("")},
		{proto.String("af1ea8ef-43f9-439a-bc74-5f5b99da59a2")},
		{proto.String(clientMCPServerID), proto.String(legacyMCPServerID)},
	} {
		initial := mcpContextSnapshot(t, ids...)
		if _, err := buildRequest("model-a", preparedRequest{Query: "hello", RootTaskContext: initial}); !errors.Is(err, errContinuationState) {
			t.Fatal("foreign or mixed installation reached the wire")
		}
		if _, err := readStreamWithContext(strings.NewReader(""), "model-a", initial); !errors.Is(err, errStreamStream) {
			t.Fatal("foreign or mixed installation entered the stream parser")
		}
	}
}

func TestMCPCallbackInstallationMustMatchDeclaredContext(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		var initial []byte
		want := clientMCPServerID
		if legacy {
			want = legacyMCPServerID
			initial = mcpContextSnapshot(t, proto.String(want))
		}
		for _, id := range []*string{nil, proto.String(clientMCPServerID), proto.String(legacyMCPServerID), proto.String("foreign-server"), proto.String("")} {
			call := integrityMCP(t, "next-message", "next-call", "echo", map[string]any{}, id)
			events := []*warpapi.ResponseEvent{initEvent(t, "conversation")}
			if len(initial) == 0 {
				events = append(events, testAction(continuationCreate()))
			}
			events = append(events, integrityTaskMessages(call), testDone())
			result, err := readStreamWithContext(strings.NewReader(integrityStream(t, events...)), "model-a", initial)
			if id == nil || *id == want {
				if err != nil || !result.Complete || len(result.ToolCalls) != 1 || result.ToolCalls[0].ID != "next-call" {
					t.Fatalf("matching callback rejected: legacy=%t id=%v err=%v", legacy, id, err)
				}
			} else if !errors.Is(err, errStreamTool) || result.Complete || !result.ToolRequested {
				t.Fatalf("foreign callback accepted: legacy=%t id=%v err=%v", legacy, id, err)
			}
		}
	}
}
