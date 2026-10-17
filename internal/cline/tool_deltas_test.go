package cline

import (
	"orchids-api/internal/upstream"
	"strings"
	"testing"
)

func TestStreamedToolArgumentsAndInvalidTerminal(t *testing.T) {
	for _, args := range []string{`{\"x\":1}`, `{`} {
		input := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\",\"function\":{\"name\":\"shell\",\"arguments\":\"" + args + "\"}}]}}]}\n\n" + "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		var events []string
		emit := func(m upstream.SSEMessage) { events = append(events, m.Type) }
		_, err := consumeStream(strings.NewReader(input), true, emit, true)
		if len(events) == 0 || events[0] != "model.tool-call-delta" {
			t.Fatalf("no early arguments: %v", events)
		}
		if args == `{` {
			if err == nil {
				t.Fatal("invalid partial JSON accepted")
			}
			if len(events) != 1 {
				t.Fatalf("invalid tool completed: %v", events)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}
