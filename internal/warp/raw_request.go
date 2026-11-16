package warp

import (
	"bytes"
	"encoding/json"

	apperrors "orchids-api/internal/errors"
)

// validateRawRequest checks the wire shape before shared decoding can discard
// unknown controls. The normalized validator still enforces conversation size,
// tool shape and model before any credential refresh or generation.
func validateRawRequest(raw json.RawMessage) error {
	reject := func() error {
		return apperrors.New("invalid_request_error", "Warp accepts model, messages, stream, tools, tool_choice and parallel_tool_calls with text-only conversation content", 400)
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil || request == nil {
		return reject()
	}
	for key := range request {
		switch key {
		case "model", "messages", "stream", "tools", "tool_choice", "parallel_tool_calls",
			"system", "max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k",
			"stop", "stop_sequences", "metadata", "thinking":
		default:
			return reject()
		}
	}
	var model string
	if !rawString(request["model"], &model) {
		return reject()
	}
	for _, key := range []string{"stream", "parallel_tool_calls"} {
		if value, exists := request[key]; exists && !rawBool(value) {
			return reject()
		}
	}
	if system, exists := request["system"]; exists {
		var text string
		if !rawString(system, &text) {
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(system, &blocks) != nil || blocks == nil {
				return reject()
			}
			for _, block := range blocks {
				if !rawContentBlock(block, "system") {
					return reject()
				}
			}
		}
	}
	if choice, exists := request["tool_choice"]; exists {
		var value any
		if json.Unmarshal(choice, &value) != nil {
			return reject()
		}
		if _, _, ok := parseToolChoice(value); value == nil || !ok {
			return reject()
		}
	}
	if tools, exists := request["tools"]; exists {
		var entries []any
		if json.Unmarshal(tools, &entries) != nil || entries == nil {
			return reject()
		}
		if _, ok := convertTools(entries); !ok {
			return reject()
		}
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil || messages == nil {
		return reject()
	}
	for _, message := range messages {
		if message == nil || !rawKeys(message, "role", "content", "tool_calls", "tool_call_id") {
			return reject()
		}
		var role, text string
		if !rawString(message["role"], &role) {
			return reject()
		}
		switch role {
		case "system", "user", "assistant":
			if _, exists := message["tool_call_id"]; exists {
				return reject()
			}
		case "tool":
			if !rawString(message["tool_call_id"], &text) || text == "" {
				return reject()
			}
		default:
			return reject()
		}
		content := bytes.TrimSpace(message["content"])
		switch {
		case len(content) == 0 || bytes.Equal(content, []byte("null")):
			// Valid only for an assistant turn that carries tool calls.
			if _, ok := message["tool_calls"]; role != "assistant" || !ok {
				return reject()
			}
		case rawString(content, &text):
		default:
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(content, &blocks) != nil || blocks == nil {
				return reject()
			}
			for _, block := range blocks {
				if !rawContentBlock(block, role) {
					return reject()
				}
			}
		}
		if calls, ok := message["tool_calls"]; ok {
			if role != "assistant" {
				return reject()
			}
			var entries []map[string]json.RawMessage
			if json.Unmarshal(calls, &entries) != nil || len(entries) == 0 {
				return reject()
			}
			for _, entry := range entries {
				if entry == nil || !rawKeys(entry, "id", "type", "function") {
					return reject()
				}
				if !rawString(entry["type"], &text) || text != "function" || !rawString(entry["id"], &text) || text == "" {
					return reject()
				}
				var fn map[string]json.RawMessage
				if json.Unmarshal(entry["function"], &fn) != nil || fn == nil || !rawKeys(fn, "name", "arguments") {
					return reject()
				}
				if !rawString(fn["name"], &text) || !validToolName(text) {
					return reject()
				}
				if args, ok := fn["arguments"]; ok {
					if !rawString(args, &text) {
						return reject()
					}
					var input map[string]any
					if json.Unmarshal([]byte(text), &input) != nil || input == nil {
						return reject()
					}
				}
			}
		}
	}
	return nil
}

func rawKeys(object map[string]json.RawMessage, allowed ...string) bool {
	for key := range object {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func rawContentBlock(block map[string]json.RawMessage, role string) bool {
	var kind, text string
	if block == nil || !rawString(block["type"], &kind) {
		return false
	}
	switch kind {
	case "text":
		return rawKeys(block, "type", "text") && rawString(block["text"], &text)
	case "tool_use":
		if role != "assistant" || !rawKeys(block, "type", "id", "name", "input") || !rawString(block["id"], &text) || text == "" || !rawString(block["name"], &text) || !validToolName(text) {
			return false
		}
		var input map[string]any
		return json.Unmarshal(block["input"], &input) == nil && input != nil
	case "tool_result":
		if role != "user" || !rawKeys(block, "type", "tool_use_id", "content", "is_error") || !rawString(block["tool_use_id"], &text) || text == "" {
			return false
		}
		if value, exists := block["is_error"]; exists && !rawBool(value) {
			return false
		}
		if rawString(block["content"], &text) {
			return true
		}
		var content []map[string]json.RawMessage
		if json.Unmarshal(block["content"], &content) != nil || content == nil {
			return false
		}
		for _, item := range content {
			if item == nil || !rawKeys(item, "type", "text") || !rawString(item["type"], &kind) || kind != "text" || !rawString(item["text"], &text) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func rawBool(raw json.RawMessage) bool {
	var value bool
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &value) == nil
}

func rawString(raw json.RawMessage, value *string) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, value) == nil
}
