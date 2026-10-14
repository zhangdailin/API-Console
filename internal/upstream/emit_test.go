package upstream

import (
	"testing"

	"orchids-api/internal/testutil"
	"orchids-api/internal/util"
)

// TestSharedEmittersMatchEveryCallersShape pins the three stream readers onto
// one implementation: the event they emit, and the counters they move, are the
// same values each package produced before the merge.
func TestSharedEmittersMatchEveryCallersShape(t *testing.T) {
	t.Parallel()

	var events []SSEMessage
	saw := false
	EmitTextDelta(func(msg SSEMessage) { events = append(events, msg) }, "", &saw)
	testutil.Falsef(t, saw || len(events) != 0, "empty delta reported: saw=%v events=%v", saw, events)
	EmitTextDelta(func(msg SSEMessage) { events = append(events, msg) }, "hi", &saw)
	testutil.Falsef(t, !saw || len(events) != 1 || events[0].Type != "model.text-delta" || events[0].Event["delta"] != "hi", "text delta = %v", events)

	// A nil callback must still count: the counter decides whether the attempt
	// was meaningful, which is independent of whether anyone is listening.
	emitted := false
	toolCount := 0
	accumulator := util.NewToolCallAccumulator()
	accumulator.Add(0, "", "Bash", `{"command":"ls"}`)
	EmitToolCalls(nil, accumulator.CompleteAll(), &emitted, &toolCount)
	testutil.Falsef(t, !emitted || toolCount != 1, "tool emit with no listener: saw=%v count=%d", emitted, toolCount)

	events = nil
	accumulator = util.NewToolCallAccumulator()
	accumulator.Add(0, "call-1", "Read", `{"path":"/tmp/a"}`)
	accumulator.Add(1, "", "Glob", `{"pattern":"*"}`)
	EmitToolCalls(func(msg SSEMessage) { events = append(events, msg) }, accumulator.CompleteAll(), &emitted, &toolCount)
	testutil.Equal(t, len(events), 2)
	testutil.Equal(t, toolCount, 3)
	for _, event := range events {
		testutil.Equal(t, event.Type, "model.tool-call")
		_, ok := event.Event["toolCallId"].(string)
		testutil.Falsef(t, !ok || event.Event["toolCallId"] == "", "tool call without an id: %+v", event.Event)
	}
	testutil.Equal(t, events[0].Event["toolName"], "Read")
	testutil.Equal(t, events[1].Event["toolName"], "Glob")

	// Draining is what makes the finish-time flush and the end-of-stream flush
	// both safe on the same accumulator.
	events = nil
	EmitToolCalls(func(msg SSEMessage) { events = append(events, msg) }, accumulator.CompleteAll(), &emitted, &toolCount)
	testutil.Equal(t, len(events), 0)

	usage := map[string]any{"inputTokens": 1}
	stored := map[string]any{}
	sawUsage := false
	ApplyStreamUsage(nil, nil, &sawUsage, &stored)
	testutil.Falsef(t, sawUsage || len(stored) != 0, "empty usage applied: saw=%v stored=%v", sawUsage, stored)
	events = nil
	ApplyStreamUsage(func(msg SSEMessage) { events = append(events, msg) }, usage, &sawUsage, &stored)
	testutil.Falsef(t, !sawUsage || stored["inputTokens"] != 1, "usage not stored: saw=%v stored=%v", sawUsage, stored)
	testutil.Equal(t, len(events), 1)
	testutil.Equal(t, events[0].Type, "model.tokens-used")
}

// TestNormalizeUsageMapUnifiesClineAndWorkBuddyAliases pins the merged
// normalizer: the reasoning count is found under every alias either channel
// accepted, and both spellings of each key are emitted because the handler
// reads either one.
func TestNormalizeUsageMapUnifiesClineAndWorkBuddyAliases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		raw  map[string]any
	}{
		{"top level snake", map[string]any{"prompt_tokens": 5.0, "completion_tokens": 2.0, "reasoning_tokens": 4.0}},
		{"top level camel", map[string]any{"promptTokens": 5.0, "completionTokens": 2.0, "reasoningTokens": 4.0}},
		{"glm thinking key", map[string]any{"prompt_tokens": 5.0, "completion_tokens": 2.0, "completion_thinking_tokens": 4.0}},
		{"nested completion details", map[string]any{
			"prompt_tokens": 5.0, "completion_tokens": 2.0,
			"completion_tokens_details": map[string]any{"reasoning_tokens": 4.0},
		}},
		{"nested camel details", map[string]any{
			"prompt_tokens": 5.0, "completion_tokens": 2.0,
			"completionTokensDetails": map[string]any{"reasoningTokens": 4.0},
		}},
		{"nested output details", map[string]any{
			"prompt_tokens": 5.0, "completion_tokens": 2.0,
			"output_tokens_details": map[string]any{"reasoning_tokens": 4.0},
		}},
		{"nested thinking alias", map[string]any{
			"prompt_tokens": 5.0, "completion_tokens": 2.0,
			"completion_tokens_details": map[string]any{"thinking_tokens": 4.0},
		}},
	}
	for _, tc := range cases {
		got := NormalizeUsageMap(tc.raw)
		testutil.Equal(t, got["reasoningTokens"], 4)
		testutil.Equal(t, got["reasoning_tokens"], 4)
		testutil.Equal(t, got["inputTokens"], 5)
		testutil.Equal(t, got["input_tokens"], 5)
		testutil.Equal(t, got["outputTokens"], 2)
		testutil.Equal(t, got["output_tokens"], 2)
	}

	got := NormalizeUsageMap(nil)
	testutil.Falsef(t, got != nil, "nil usage = %+v, want nil", got)
	// A usage object with neither input nor output is not a usage report.
	got = NormalizeUsageMap(map[string]any{"total_tokens": 9.0})
	testutil.Falsef(t, got != nil, "usage without input/output = %+v, want nil", got)
	got = NormalizeUsageMap(map[string]any{"prompt_tokens": 3.0, "prompt_cache_hit_tokens": 2.0})
	testutil.Falsef(t, got["cacheReadTokens"] != 2 || got["cache_read_tokens"] != 2, "cache read = %+v", got)
}
