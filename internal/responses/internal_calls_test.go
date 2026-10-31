package responses

import (
	"testing"

	"orchids-api/internal/testutil"
)

// liveInternalCall is the shape grok-4.7 returned on 2026-11-01 while running
// a native x_search.
func liveInternalCall(id, name string, n int) map[string]any {
	return map[string]any{"type": "custom_tool_call", "id": id, "name": name, "status": "completed",
		"call_id": "xs_call-4be284ca-207f-4e98-823a-e499b412a574-" + string(rune('0'+n)), "input": `{"query":"xai"}`}
}

func TestIsInternalSearchCallMatchesOnlyUpstreamSubcalls(t *testing.T) {
	testutil.True(t, IsInternalSearchCall(liveInternalCall("ctc_1", "x_keyword_search", 0)), "a live subcall is internal")
	testutil.True(t, IsInternalSearchCall(map[string]any{"type": "custom_tool_call", "name": "x_thread_fetch", "call_id": "c"}), "an internal name is internal")
	testutil.True(t, IsInternalSearchCall(map[string]any{"type": "custom_tool_call", "name": "other", "call_id": "xs_call-1"}), "an xs_call id is internal")
	testutil.False(t, IsInternalSearchCall(map[string]any{"type": "function_call", "name": "x_keyword_search", "call_id": "c"}), "a function call is the client's")
	testutil.False(t, IsInternalSearchCall(map[string]any{"type": "web_search_call"}), "a search call item is kept")
}

func TestFilterInternalSearchCallsKeepsEverythingElse(t *testing.T) {
	response := map[string]any{"output": []any{
		map[string]any{"type": "reasoning"},
		liveInternalCall("ctc_1", "x_keyword_search", 0),
		map[string]any{"type": "web_search_call"},
		liveInternalCall("ctc_2", "x_user_search", 1),
		map[string]any{"type": "message"},
	}}
	testutil.True(t, FilterInternalSearchCalls(response), "subcalls must be removed")
	output := InterfaceMaps(response["output"])
	testutil.Equal(t, len(output), 3)
	testutil.Equal[any](t, output[2]["type"], "message")
	testutil.False(t, FilterInternalSearchCalls(response), "a clean output is unchanged")
}

// TestInternalCallStreamFilterRenumbersOutput drops a subcall's events and
// closes the gap it leaves in output_index.
func TestInternalCallStreamFilterRenumbersOutput(t *testing.T) {
	filter := NewInternalCallStreamFilter()
	call := liveInternalCall("ctc_1", "x_keyword_search", 0)
	drop, _ := filter.Apply(map[string]any{"type": "response.output_item.added", "output_index": float64(1), "item": call})
	testutil.True(t, drop, "the subcall is dropped when added")
	drop, _ = filter.Apply(map[string]any{"type": "response.custom_tool_call_input.delta", "item_id": "ctc_1", "output_index": float64(1)})
	testutil.True(t, drop, "its input stream is dropped")
	drop, _ = filter.Apply(map[string]any{"type": "response.output_item.done", "output_index": float64(1), "item": call})
	testutil.True(t, drop, "and its completion")

	before := map[string]any{"type": "response.output_item.added", "output_index": float64(0), "item": map[string]any{"type": "reasoning"}}
	drop, changed := filter.Apply(before)
	testutil.False(t, drop || changed, "an earlier item is untouched")
	after := map[string]any{"type": "response.output_text.delta", "output_index": float64(2), "delta": "hi"}
	drop, changed = filter.Apply(after)
	testutil.False(t, drop, "a later item is kept")
	testutil.True(t, changed, "and renumbered")
	testutil.Equal[any](t, after["output_index"], 1)

	completed := map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "reasoning"}, call, map[string]any{"type": "message"}}}}
	_, changed = filter.Apply(completed)
	testutil.True(t, changed, "the terminal output is filtered")
	testutil.Equal(t, len(InterfaceMaps(completed["response"].(map[string]any)["output"])), 2)
}
