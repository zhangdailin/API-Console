package responses

import "strings"

// While the upstream runs a native x_search it reports each internal subcall
// as a completed custom_tool_call (x_keyword_search, x_user_search, ...; call
// ids prefixed xs_call). The upstream has already executed them, so a client
// that sees one would try to run a tool it never declared. They are removed
// from the client-visible protocol. Client custom tools are lowered to
// functions before they reach the upstream, so an upstream custom_tool_call
// is never the client's own.
var internalSearchToolNames = map[string]bool{
	"x_keyword_search": true, "x_user_search": true, "x_semantic_search": true, "x_thread_fetch": true,
}

// IsInternalSearchCall reports whether an upstream output item is an internal
// search subcall.
func IsInternalSearchCall(item map[string]any) bool {
	if item == nil || item["type"] != "custom_tool_call" {
		return false
	}
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	return strings.HasPrefix(strings.TrimSpace(callID), "xs_call") || internalSearchToolNames[strings.TrimSpace(name)]
}

// FilterInternalSearchCalls removes internal subcalls from a response object's
// output. It reports whether anything was removed.
func FilterInternalSearchCalls(response map[string]any) bool {
	output, ok := response["output"].([]any)
	if !ok {
		return false
	}
	kept := make([]any, 0, len(output))
	for _, raw := range output {
		if item, ok := raw.(map[string]any); ok && IsInternalSearchCall(item) {
			continue
		}
		kept = append(kept, raw)
	}
	if len(kept) == len(output) {
		return false
	}
	response["output"] = kept
	return true
}

// InternalCallStreamFilter removes internal subcalls from an event stream and
// renumbers output_index so the client sees a contiguous output list.
type InternalCallStreamFilter struct {
	items   map[string]bool
	dropped []int
}

func NewInternalCallStreamFilter() *InternalCallStreamFilter {
	return &InternalCallStreamFilter{items: map[string]bool{}}
}

// Apply reports whether the event must be dropped. A kept event is adjusted in
// place: its output_index is renumbered and a terminal response's output list
// is filtered. It returns changed when it modified a kept event.
func (f *InternalCallStreamFilter) Apply(event map[string]any) (drop, changed bool) {
	if f == nil || event == nil {
		return false, false
	}
	kind, _ := event["type"].(string)
	switch kind {
	case "response.output_item.added", "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if IsInternalSearchCall(item) {
			if id, _ := item["id"].(string); id != "" {
				f.items[id] = true
			}
			if index, ok := eventOutputIndex(event); ok && kind == "response.output_item.added" {
				f.dropped = append(f.dropped, index)
			}
			return true, false
		}
	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		if id, _ := event["item_id"].(string); f.items[id] {
			return true, false
		}
	}
	if response, ok := event["response"].(map[string]any); ok && FilterInternalSearchCalls(response) {
		changed = true
	}
	if index, ok := eventOutputIndex(event); ok && len(f.dropped) > 0 {
		shift := 0
		for _, dropped := range f.dropped {
			if dropped < index {
				shift++
			}
		}
		if shift > 0 {
			event["output_index"] = index - shift
			changed = true
		}
	}
	return false, changed
}

func eventOutputIndex(event map[string]any) (int, bool) {
	switch value := event["output_index"].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case int64:
		return int(value), true
	}
	return 0, false
}
