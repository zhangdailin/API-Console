package warp

import (
	"errors"
	warpapi "github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestContinuationAllowsOnlyPreviouslyIssuedMCPResultEcho(t *testing.T) {
	ctx := newContinuationAccumulator(nil)
	create := continuationCreate()
	continuationApply(t, ctx, create)
	call := mcpToolCallEvent(t, "issued", "echo", map[string]any{"text": "OK"})
	continuationApply(t, ctx, call.GetClientActions().GetActions()[0])
	initial, err := ctx.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"issued", "unbound"} {
		msg := warpapi.Message_builder{Id: proto.String("result-message"), TaskId: proto.String("root"), ToolCallResult: warpapi.Message_ToolCallResult_builder{ToolCallId: proto.String(id), CallMcpTool: warpapi.CallMCPToolResult_builder{Success: warpapi.CallMCPToolResult_Success_builder{}.Build()}.Build()}.Build()}.Build()
		stream := testSSE(t, integrityTaskMessages(msg)) + testSSE(t, integrityTaskMessages(continuationText("reply", "OK"))) + testSSE(t, testDone())
		result, err := readStreamWithContext(strings.NewReader(stream), "model-a", initial)
		if id == "issued" {
			if err != nil || result.Text != "OK" {
				t.Fatalf("expected echo rejected: %v", err)
			}
		} else if !errors.Is(err, errStreamTool) {
			t.Fatal("unbound result accepted")
		}
	}
}
