package grok

import (
	"fmt"
	"orchids-api/internal/chatwire"
	"strings"
	"time"
)

// responsesCompatibilityState carries the small amount of identity needed to
// supplement Build events for strict Responses clients. It deliberately does
// not canonicalize events: a complete event is returned byte-for-byte by the
// caller.
type responsesCompatibilityState struct {
	responseID string
	createdAt  int64
	model      string
	itemSeq    int
	itemIDs    map[int64]string
}

func supplementResponsesEvent(event map[string]interface{}, state *responsesCompatibilityState) bool {
	if event == nil || state == nil {
		return false
	}
	changed := false
	kind := chatwire.ParseLooseStringAny(event["type"])
	response, _ := event["response"].(map[string]interface{})
	if response != nil {
		id := strings.TrimSpace(chatwire.ParseLooseStringAny(response["id"]))
		if id == "" {
			id = state.ensureResponseID()
			response["id"] = id
			changed = true
		} else if state.responseID == "" {
			state.responseID = id
		}
		if _, ok := response["created_at"]; !ok || response["created_at"] == nil {
			if state.createdAt == 0 {
				state.createdAt = time.Now().Unix()
			}
			response["created_at"] = state.createdAt
			changed = true
		}
		if _, ok := response["object"]; !ok || response["object"] == nil {
			response["object"] = "response"
			changed = true
		}
		if _, ok := response["output"]; !ok || response["output"] == nil {
			response["output"] = []interface{}{}
			changed = true
		}
		if model := strings.TrimSpace(chatwire.ParseLooseStringAny(response["model"])); model != "" {
			state.model = model
		} else {
			response["model"] = state.model
			changed = true
		}
	}
	if item, ok := event["item"].(map[string]interface{}); ok {
		index, indexed := responseEventIndex(event["output_index"])
		id := strings.TrimSpace(chatwire.ParseLooseStringAny(item["id"]))
		if indexed && state.itemIDs != nil && state.itemIDs[index] != "" {
			id = state.itemIDs[index]
		}
		if id == "" {
			id = state.nextItemID()
		}
		if chatwire.ParseLooseStringAny(item["id"]) != id {
			item["id"] = id
			changed = true
		}
		if indexed {
			if state.itemIDs == nil {
				state.itemIDs = map[int64]string{}
			}
			if state.itemIDs[index] == "" {
				state.itemIDs[index] = id
			}
		}
	}
	if responsesEventNeedsItemID(kind) && strings.TrimSpace(chatwire.ParseLooseStringAny(event["item_id"])) == "" {
		if index, ok := responseEventIndex(event["output_index"]); ok {
			id := ""
			if state.itemIDs != nil {
				id = state.itemIDs[index]
			}
			if id == "" {
				id = state.nextItemID()
				if state.itemIDs == nil {
					state.itemIDs = map[int64]string{}
				}
				state.itemIDs[index] = id
			}
			event["item_id"] = id
			changed = true
		}
	}
	if responsesEventCarriesResponseID(kind) {
		id := strings.TrimSpace(chatwire.ParseLooseStringAny(event["id"]))
		if id == "" {
			event["id"] = state.ensureResponseID()
			changed = true
		} else if state.responseID == "" {
			state.responseID = id
		}
	}
	if ensureResponsesOutputTextAnnotations(event) {
		changed = true
	}
	return changed
}

func (s *responsesCompatibilityState) ensureResponseID() string {
	if s.responseID == "" {
		s.responseID = "resp_compat"
	}
	return s.responseID
}

func (s *responsesCompatibilityState) nextItemID() string {
	s.itemSeq++
	return fmt.Sprintf("item_%d", s.itemSeq)
}

func responseEventIndex(value interface{}) (int64, bool) {
	switch value := value.(type) {
	case float64:
		return int64(value), true
	case int:
		return int64(value), true
	case int64:
		return value, true
	default:
		return 0, false
	}
}

func responsesEventCarriesResponseID(kind string) bool {
	switch kind {
	case "response.created", "response.in_progress", "response.completed", "response.incomplete", "response.failed":
		return true
	default:
		return false
	}
}

func responsesEventNeedsItemID(kind string) bool {
	switch kind {
	case "response.output_text.delta", "response.output_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.refusal.delta", "response.refusal.done",
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return true
	default:
		return false
	}
}

func ensureResponsesOutputTextAnnotations(value interface{}) bool {
	changed := false
	switch value := value.(type) {
	case map[string]interface{}:
		if chatwire.ParseLooseStringAny(value["type"]) == "output_text" {
			if _, ok := value["annotations"]; !ok || value["annotations"] == nil {
				value["annotations"] = []interface{}{}
				changed = true
			}
		}
		for _, key := range []string{"item", "part", "response", "content", "output", "delta"} {
			if ensureResponsesOutputTextAnnotations(value[key]) {
				changed = true
			}
		}
	case []interface{}:
		for _, child := range value {
			if ensureResponsesOutputTextAnnotations(child) {
				changed = true
			}
		}
	}
	return changed
}
