package qoder

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestStreamPrefixPreservesNativeToolsAndOrder(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"The", "Available upstream", " "} {
		for _, split := range []bool{false, true} {
			t.Run(fmt.Sprintf("prefix=%q/split=%v", prefix, split), func(t *testing.T) {
				body := ""
				content := prefix
				if split {
					body = envelope(`{"choices":[{"delta":{"content":` + jsonString(prefix) + `}}]}`)
					content = ""
				}
				body += envelope(`{"choices":[{"delta":{"content":` + jsonString(content) + `,"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"key\":"}}]}}]}`)
				body += envelope(`{"choices":[{"delta":{"content":"after","tool_calls":[{"index":0,"function":{"arguments":"1}"}}]},"finish_reason":"length"}]}`)
				body += "event:finish\n\n"
				var events []upstream.SSEMessage
				result, err := consumeStreamObserved(strings.NewReader(body), func(e upstream.SSEMessage) { events = append(events, e) }, nil)
				testutil.NoError(t, err)
				testutil.Falsef(t, result.ToolCallCount != 1 || result.FinishReasonValue != "length" || result.FinishReason() != "max_tokens", "result = %+v", result)
				testutil.Falsef(t, len(events) != 3 || events[0].Type != "model.text-delta" || events[0].Event["delta"] != prefix || events[1].Type != "model.text-delta" || events[1].Event["delta"] != "after" || events[2].Type != "model.tool-call", "events = %+v", events)
				if events[2].Event["toolCallId"] != "call_1" || events[2].Event["toolName"] != "lookup" || events[2].Event["input"] != `{"key":1}` {
					t.Fatalf("tool = %+v", events[2])
				}
			})
		}
	}
}

func TestStreamPrefixSharedFinishFrame(t *testing.T) {
	t.Parallel()
	for _, withTool := range []bool{false, true} {
		for _, terminated := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/terminated=%v", withTool, terminated), func(t *testing.T) {
				tool := ""
				if withTool {
					tool = `,"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{}"}}]`
				}
				body := envelope(`{"choices":[{"delta":{"content":"The"` + tool + `},"finish_reason":"length"}]}`)
				if terminated {
					body += "data: [DONE]\n\nevent:finish\n\n"
				}
				var events []upstream.SSEMessage
				result, err := consumeStreamObserved(strings.NewReader(body), func(e upstream.SSEMessage) { events = append(events, e) }, nil)
				testutil.Falsef(t, (terminated && err != nil) || (!terminated && !errors.Is(err, ErrStreamTruncated)), "error = %v", err)
				wantEvents := 1
				if withTool {
					wantEvents++
				}
				testutil.Falsef(t, len(events) != wantEvents || events[0].Type != "model.text-delta" || events[0].Event["delta"] != "The", "events = %+v", events)
				testutil.Equal(t, result.FinishReasonValue, "length")
				testutil.Equal(t, result.FinishReason(), "max_tokens")
				testutil.Falsef(t, withTool && (result.ToolCallCount != 1 || events[1].Type != "model.tool-call"), "tool lost: %+v, %+v", result, events)
			})
		}
	}
}
