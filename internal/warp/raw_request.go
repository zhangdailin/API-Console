package warp

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"unicode/utf8"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
)

// wireFieldError accepts only paths and reasons constructed by this package.
// Never include a client-supplied field name or value in a public error.
func wireFieldError(path, reason string) error {
	return apperrors.New("invalid_request_error", "Warp request field "+path+": "+reason, 400)
}

// validateRawRequest checks the wire shape before shared decoding can discard
// unknown controls. The normalized validator still enforces conversation shape,
// tool shape and model before any credential refresh or generation.
func validateRawRequest(raw json.RawMessage) error {
	var request map[string]json.RawMessage
	if json.Unmarshal(raw, &request) != nil || request == nil {
		return wireFieldError("request", "must be a JSON object")
	}
	// Reject nonempty safeguards before other hints: Warp cannot enforce their
	// semantics, even when another unsupported control is present as well.
	if value, exists := request["safeguards"]; exists {
		var safeguards []json.RawMessage
		if json.Unmarshal(value, &safeguards) != nil || safeguards == nil {
			return wireFieldError("safeguards", "must be an empty array; safeguards are unsupported and Warp cannot enforce them")
		}
		if len(safeguards) != 0 {
			return wireFieldError("safeguards", "unsupported; Warp cannot enforce safeguards")
		}
	}
	for _, key := range sortedRawKeys(request) {
		switch key {
		case "model", "messages", "stream", "tools", "tool_choice", "parallel_tool_calls",
			"system", "max_tokens", "max_completion_tokens", "temperature", "top_p", "top_k",
			"stop", "stop_sequences", "metadata", "thinking", "output_config", "context_management", "safeguards",
			"store", "stream_options":
		default:
			return wireFieldError(rawTopFieldPath(key), "unsupported request field")
		}
	}
	if err := rawClientOptions(request); err != nil {
		return err
	}
	if value, exists := request["context_management"]; exists {
		if err := rawContextManagement(value); err != nil {
			return err
		}
	}
	// Anthropic compatibility hints are validated but never forwarded. Warp
	// exposes neither reasoning effort nor output-format or prompt-cache control.
	if output, exists := request["output_config"]; exists {
		var config map[string]json.RawMessage
		if json.Unmarshal(output, &config) != nil || config == nil || len(config) == 0 {
			return wireFieldError("output_config", "must be a nonempty object")
		}
		if _, hasFormat := config["format"]; hasFormat {
			return apperrors.New("invalid_request_error", "Warp does not support output_config.format; only output_config.effort is accepted", 400)
		}
		if len(config) != 1 {
			return wireFieldError("output_config", "only effort is accepted")
		}
		var effort string
		if !rawString(config["effort"], &effort) || len(effort) == 0 || len(effort) > 32 || !utf8.ValidString(effort) {
			return wireFieldError("output_config.effort", "must be a nonempty string of at most 32 bytes")
		}
	}
	if thinking, exists := request["thinking"]; exists {
		var config map[string]json.RawMessage
		if json.Unmarshal(thinking, &config) != nil || config == nil || len(config) == 0 {
			return wireFieldError("thinking", "must be a nonempty object")
		}
		var kind string
		if !rawString(config["type"], &kind) || (kind != "enabled" && kind != "disabled" && kind != "adaptive") {
			return wireFieldError("thinking.type", "must be enabled, disabled or adaptive")
		}
		for _, key := range sortedRawKeys(config) {
			value := config[key]
			switch key {
			case "type":
			case "budget_tokens":
				if !rawNonNegativeInt(value) {
					return wireFieldError("thinking.budget_tokens", "must be a nonnegative integer")
				}
			case "display":
				var display string
				if kind == "disabled" || !rawString(value, &display) || (display != "omitted" && display != "summarized") {
					return wireFieldError("thinking.display", "must be omitted or summarized with enabled or adaptive thinking")
				}
			default:
				return wireFieldError("thinking", "unsupported field")
			}
		}
	}
	if metadata, exists := request["metadata"]; exists {
		var fields map[string]json.RawMessage
		if json.Unmarshal(metadata, &fields) != nil || fields == nil {
			return wireFieldError("metadata", "must be an object")
		}
		for _, key := range sortedRawKeys(fields) {
			if key != "user_id" {
				return wireFieldError("metadata", "unsupported field")
			}
			var userID string
			if !rawString(fields[key], &userID) || len(userID) == 0 || len(userID) > 256 || !utf8.ValidString(userID) {
				return wireFieldError("metadata.user_id", "must be a nonempty string of at most 256 bytes")
			}
		}
	}
	var model string
	if !rawString(request["model"], &model) {
		return wireFieldError("model", "must be a string")
	}
	for _, key := range []string{"stream", "parallel_tool_calls"} {
		if value, exists := request[key]; exists && !rawBool(value) {
			return wireFieldError(key, "must be a boolean")
		}
	}
	if system, exists := request["system"]; exists {
		var text string
		if !rawString(system, &text) {
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(system, &blocks) != nil || blocks == nil {
				return wireFieldError("system", "must be text or an array of text blocks")
			}
			for i, block := range blocks {
				if err := rawContentBlock(block, "system", "system["+strconv.Itoa(i)+"]"); err != nil {
					return err
				}
			}
		}
	}
	if choice, exists := request["tool_choice"]; exists {
		var value any
		if json.Unmarshal(choice, &value) != nil {
			return wireFieldError("tool_choice", "invalid tool choice")
		}
		if _, _, ok := parseToolChoice(value); value == nil || !ok {
			return wireFieldError("tool_choice", "unsupported tool choice")
		}
	}
	if tools, exists := request["tools"]; exists {
		var entries []any
		if json.Unmarshal(tools, &entries) != nil || entries == nil {
			return wireFieldError("tools", "must be an array")
		}
		if _, err := convertToolsWithError(entries); err != nil {
			return err
		}
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil || messages == nil {
		return wireFieldError("messages", "must be an array")
	}
	for i, message := range messages {
		path := "messages[" + strconv.Itoa(i) + "]"
		if message == nil || !rawKeys(message, "role", "content", "tool_calls", "tool_call_id") {
			return wireFieldError(path, "must be an object with supported message fields")
		}
		var role, text string
		if !rawString(message["role"], &role) {
			return wireFieldError(path+".role", "must be a string")
		}
		switch role {
		case "system", "developer", "user", "assistant":
			if _, exists := message["tool_call_id"]; exists {
				return wireFieldError(path+".tool_call_id", "only supported for tool messages")
			}
		case "tool":
			if !rawString(message["tool_call_id"], &text) || text == "" {
				return wireFieldError(path+".tool_call_id", "must be a nonempty string")
			}
		default:
			return wireFieldError(path+".role", "unsupported role")
		}
		content := bytes.TrimSpace(message["content"])
		switch {
		case len(content) == 0 || bytes.Equal(content, []byte("null")):
			if _, ok := message["tool_calls"]; role != "assistant" || !ok {
				return wireFieldError(path+".content", "required except for assistant tool calls")
			}
		case rawString(content, &text):
		default:
			var blocks []map[string]json.RawMessage
			if json.Unmarshal(content, &blocks) != nil || blocks == nil {
				return wireFieldError(path+".content", "must be text or an array of supported blocks")
			}
			for j, block := range blocks {
				if err := rawContentBlock(block, role, path+".content["+strconv.Itoa(j)+"]"); err != nil {
					return err
				}
			}
		}
		if calls, ok := message["tool_calls"]; ok {
			callPath := path + ".tool_calls"
			if role != "assistant" {
				return wireFieldError(callPath, "only supported for assistant messages")
			}
			var entries []map[string]json.RawMessage
			if json.Unmarshal(calls, &entries) != nil || len(entries) == 0 {
				return wireFieldError(callPath, "must be a nonempty array")
			}
			for j, entry := range entries {
				entryPath := callPath + "[" + strconv.Itoa(j) + "]"
				if entry == nil || !rawKeys(entry, "id", "type", "function") {
					return wireFieldError(entryPath, "unsupported tool call fields")
				}
				if !rawString(entry["type"], &text) || text != "function" {
					return wireFieldError(entryPath+".type", "must be function")
				}
				if !rawString(entry["id"], &text) || text == "" {
					return wireFieldError(entryPath+".id", "must be a nonempty string")
				}
				var fn map[string]json.RawMessage
				if json.Unmarshal(entry["function"], &fn) != nil || fn == nil || !rawKeys(fn, "name", "arguments") {
					return wireFieldError(entryPath+".function", "unsupported function fields")
				}
				if !rawString(fn["name"], &text) || !validToolName(text) {
					return wireFieldError(entryPath+".function.name", "invalid tool name")
				}
				if args, ok := fn["arguments"]; ok {
					if !rawString(args, &text) {
						return wireFieldError(entryPath+".function.arguments", "must encode a JSON object")
					}
					var input map[string]any
					if json.Unmarshal([]byte(text), &input) != nil || input == nil {
						return wireFieldError(entryPath+".function.arguments", "must encode a JSON object")
					}
				}
			}
		}
	}
	// Validate instructions before selection so their fixed safe error is
	// returned directly instead of being scrubbed as an upstream client error.
	var instructions []prompt.SystemItem
	if raw, exists := request["system"]; exists {
		var text string
		if rawString(raw, &text) {
			instructions = []prompt.SystemItem{{Type: "text", Text: text}}
		} else if json.Unmarshal(raw, &instructions) != nil {
			return wireFieldError("system", "invalid instructions")
		}
	}
	var normalized []prompt.Message
	if json.Unmarshal(request["messages"], &normalized) != nil {
		return wireFieldError("messages", "invalid message content")
	}
	if err := validateThinkingHistory(normalized); err != nil {
		return err
	}
	if _, _, err := normalizeInstructions(instructions, normalized); err != nil {
		return err
	}
	return nil
}

// Only this exact keep-all edit is a no-op. In particular clear_tool_uses must
// never be accepted and silently discarded by shared normalization.
func rawContextManagement(raw json.RawMessage) error {
	var config map[string]json.RawMessage
	if json.Unmarshal(raw, &config) != nil || config == nil || !rawKeys(config, "edits") {
		return wireFieldError("context_management", "must be an object containing only edits")
	}
	var edits []map[string]json.RawMessage
	if json.Unmarshal(config["edits"], &edits) != nil || edits == nil || len(edits) > 1 {
		return wireFieldError("context_management.edits", "unsupported; only an empty array or one clear_thinking_20251015 keep-all no-op is accepted")
	}
	if len(edits) == 0 {
		return nil
	}
	edit := edits[0]
	var kind, keep string
	if edit == nil || len(edit) != 2 || !rawKeys(edit, "type", "keep") || !rawString(edit["type"], &kind) || kind != "clear_thinking_20251015" || !rawString(edit["keep"], &keep) || keep != "all" {
		return wireFieldError("context_management.edits", "unsupported; only clear_thinking_20251015 with keep all is a no-op")
	}
	return nil
}

func sortedRawKeys(object map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// This static mapping keeps diagnostics useful without reflecting arbitrary
// client-controlled keys (which may contain secrets or control characters).
func rawTopFieldPath(key string) string {
	switch key {
	case "n", "seed", "penalties", "presence_penalty", "frequency_penalty", "logprobs", "top_logprobs", "logit_bias",
		"web_search_options", "reasoning", "reasoning_effort", "verbosity", "max_output_tokens", "functions", "function_call", "mcp_servers",
		"response_format", "text", "include", "stream_options", "modalities", "audio", "prediction", "client_metadata", "conversation_id",
		"prompt_cache_key", "instructions", "store", "user", "service_tier", "background":
		return key
	default:
		return "request"
	}
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

func rawContentBlock(block map[string]json.RawMessage, role, path string) error {
	var kind, text string
	if block == nil || !rawString(block["type"], &kind) {
		return wireFieldError(path+".type", "must be a supported content type")
	}
	switch kind {
	case "thinking":
		if role != "assistant" || !rawKeys(block, "type", "thinking", "signature") {
			return wireFieldError(path, "thinking is supported only as assistant history without extra fields")
		}
		if !rawString(block["thinking"], &text) || !utf8.ValidString(text) {
			return wireFieldError(path+".thinking", "must be a valid text string")
		}
		if signature, exists := block["signature"]; exists {
			if !rawString(signature, &text) || !utf8.ValidString(text) {
				return wireFieldError(path+".signature", "must be a valid string")
			}
		}
		return nil
	case "text":
		if role != "system" && role != "developer" && role != "user" && role != "assistant" {
			return wireFieldError(path, "text blocks are unsupported for this role")
		}
		if !rawKeys(block, "type", "text", "cache_control") {
			return wireFieldError(path, "unsupported text block field")
		}
		if !rawString(block["text"], &text) {
			return wireFieldError(path+".text", "must be a string")
		}
		if cache, exists := block["cache_control"]; exists {
			return rawCacheControl(cache, path+".cache_control")
		}
		return nil
	case "tool_use":
		if role != "assistant" || !rawKeys(block, "type", "id", "name", "input") {
			return wireFieldError(path, "unsupported tool_use fields or role")
		}
		if !rawString(block["id"], &text) || text == "" {
			return wireFieldError(path+".id", "must be a nonempty string")
		}
		if !rawString(block["name"], &text) || !validToolName(text) {
			return wireFieldError(path+".name", "invalid tool name")
		}
		var input map[string]any
		if json.Unmarshal(block["input"], &input) != nil || input == nil {
			return wireFieldError(path+".input", "must be a JSON object")
		}
		return nil
	case "tool_result":
		if role != "user" || !rawKeys(block, "type", "tool_use_id", "content", "is_error", "cache_control") {
			return wireFieldError(path, "unsupported tool_result fields or role")
		}
		if !rawString(block["tool_use_id"], &text) || text == "" {
			return wireFieldError(path+".tool_use_id", "must be a nonempty string")
		}
		if value, exists := block["is_error"]; exists && !rawBool(value) {
			return wireFieldError(path+".is_error", "must be a boolean")
		}
		if cache, exists := block["cache_control"]; exists {
			if err := rawCacheControl(cache, path+".cache_control"); err != nil {
				return err
			}
		}
		if rawString(block["content"], &text) {
			return nil
		}
		var content []map[string]json.RawMessage
		if json.Unmarshal(block["content"], &content) != nil || content == nil {
			return wireFieldError(path+".content", "must be text or an array of text blocks")
		}
		for i, item := range content {
			if item == nil || !rawKeys(item, "type", "text") || !rawString(item["type"], &kind) || kind != "text" || !rawString(item["text"], &text) {
				return wireFieldError(path+".content["+strconv.Itoa(i)+"]", "must be a plain text block")
			}
		}
		return nil
	default:
		return wireFieldError(path+".type", "unsupported content type")
	}
}

func rawCacheControl(raw json.RawMessage, path string) error {
	var cache map[string]json.RawMessage
	if json.Unmarshal(raw, &cache) != nil || cache == nil {
		return wireFieldError(path, "must be an ephemeral cache-control object")
	}
	if !rawKeys(cache, "type", "ttl") {
		return wireFieldError(path, "unsupported cache-control field")
	}
	var kind string
	if !rawString(cache["type"], &kind) || kind != "ephemeral" {
		return wireFieldError(path+".type", "must be ephemeral")
	}
	if value, exists := cache["ttl"]; exists {
		var ttl string
		if !rawString(value, &ttl) || (ttl != "5m" && ttl != "1h") {
			return wireFieldError(path+".ttl", "must be 5m or 1h")
		}
	}
	return nil
}

func rawBool(raw json.RawMessage) bool {
	var value bool
	return !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) && json.Unmarshal(raw, &value) == nil
}

func rawString(raw json.RawMessage, value *string) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '"' && json.Unmarshal(raw, value) == nil
}

// rawNonNegativeInt accepts only a JSON integer >= 0 (no float, exponent,
// string, boolean, null or overflow). It is used for thinking.budget_tokens.
func rawNonNegativeInt(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return false
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < 0 {
		return false
	}
	return true
}
