package grok

import (
	"strings"

	"orchids-api/internal/responses"
)

// hideReasoning controls client-visible output only. Upstream generation,
// accounting, quality checks and the private replay cache retain reasoning.
func (h *Handler) hideReasoning() bool {
	cfg := h.configSnapshot()
	return cfg != nil && cfg.GrokHideReasoning
}

func hideChatReasoning(message map[string]interface{}) bool {
	changed := false
	for _, key := range []string{"reasoning_content", "reasoning", "reasoning_item_id", "reasoning_encrypted_content", "reasoning_done", "x_grok_reasoning"} {
		if _, exists := message[key]; exists {
			delete(message, key)
			changed = true
		}
	}
	return changed
}

// hideNativeReasoning visits only protocol reasoning items, never ordinary
// text, tool parameters or user input. Keep item identity and encrypted_content
// so native clients can continue a tool turn without readable thinking text.
func hideNativeReasoning(envelope map[string]interface{}) bool {
	if envelope == nil {
		return false
	}
	changed := hideNativeReasoningItem(envelope)
	if item, _ := envelope["item"].(map[string]interface{}); item != nil {
		changed = hideNativeReasoningItem(item) || changed
	}
	if response, _ := envelope["response"].(map[string]interface{}); response != nil {
		changed = hideNativeReasoning(response) || changed
	}
	for _, value := range responses.InterfaceSlice(envelope["output"]) {
		if item, _ := value.(map[string]interface{}); item != nil {
			changed = hideNativeReasoningItem(item) || changed
		}
	}
	return changed
}

func hideNativeReasoningItem(item map[string]interface{}) bool {
	if item["type"] != "reasoning" {
		return false
	}
	changed := false
	for _, key := range []string{"summary", "content"} {
		if _, exists := item[key]; exists {
			item[key] = []interface{}{}
			changed = true
		}
	}
	for _, key := range []string{"text", "reasoning_text"} {
		if _, exists := item[key]; exists {
			delete(item, key)
			changed = true
		}
	}
	return changed
}

func hiddenReasoningEvent(kind string, event map[string]interface{}) bool {
	if strings.HasPrefix(kind, "response.reasoning") {
		return true
	}
	if part, _ := event["part"].(map[string]interface{}); part != nil {
		return part["type"] == "reasoning_text" || part["type"] == "summary_text"
	}
	return false
}
