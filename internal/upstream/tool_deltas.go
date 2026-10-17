package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"orchids-api/internal/util"
)

type toolDeltasKey struct{}

func WithToolDeltas(ctx context.Context, enabled bool) context.Context {
	return context.WithValue(ctx, toolDeltasKey{}, enabled)
}
func ToolDeltasEnabled(ctx context.Context) bool {
	enabled, _ := ctx.Value(toolDeltasKey{}).(bool)
	return enabled
}

// EmitToolDelta waits for stable identity before publishing accumulated arguments.
// write_stdin remains buffered because Cline may repair its control characters.
func EmitToolDelta(emit func(SSEMessage), call *util.ToolCall) {
	if emit == nil || call.ID == "" || call.Name == "" || call.Name == "write_stdin" {
		return
	}
	delta := call.Arguments[call.StreamedBytes:]
	if call.StreamStarted && delta == "" {
		return
	}
	call.StreamStarted = true
	call.StreamedBytes = len(call.Arguments)
	emit(SSEMessage{Type: "model.tool-call-delta", Event: map[string]any{"toolCallId": call.ID, "toolName": call.Name, "delta": delta}})
}

func ValidateStreamedTools(calls []*util.ToolCall) error {
	for _, call := range calls {
		if !call.StreamStarted {
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(call.Arguments), &object) != nil || object == nil {
			return fmt.Errorf("upstream returned invalid streamed tool arguments")
		}
	}
	return nil
}
