package grok

import (
	"orchids-api/internal/chatwire"
	"orchids-api/internal/util"
	"slices"
	"strings"
)

// Normalized reasoning replay items.
//
// A session stores the previous turn's portable output items rather than a bare
// cipher: an opaque reasoning item alone cannot restore a turn whose assistant
// message or tool calls the next request omits. Items are normalized down to
// their portable wire shape, and replay filters them against the request input
// so nothing is ever duplicated.

const replayToolCallSeparator = "\x00"

// extractReplayItems pulls the portable item types out of a completed Responses
// object. It reports false when the payload carries no usable output at all, so
// a truncated or error payload never overwrites good state.
func extractReplayItems(raw map[string]interface{}) ([]interface{}, bool) {
	if raw == nil {
		return nil, false
	}
	outputRaw := raw["output"]
	if outputRaw == nil {
		if nested, ok := raw["response"].(map[string]interface{}); ok {
			outputRaw = nested["output"]
		}
	}
	output, ok := outputRaw.([]interface{})
	if !ok {
		return nil, false
	}
	items := make([]interface{}, 0, len(output))
	for _, entry := range output {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])) {
		case "reasoning", "message", "function_call", "custom_tool_call":
			items = append(items, item)
		}
	}
	return items, true
}

// normalizeReplayItems reduces items to their portable shape. It reports false
// when nothing anchorable survives, which tells the caller to drop the session
// state rather than replay a partial turn.
func normalizeReplayItems(items []interface{}) ([]interface{}, bool) {
	normalized := make([]interface{}, 0, len(items))
	hasAnchor := false
	for _, entry := range items {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		next, ok := normalizeReplayItem(item)
		if !ok {
			continue
		}
		normalized = append(normalized, next)
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(next["type"])) {
		case "reasoning", "function_call", "custom_tool_call":
			hasAnchor = true
		}
	}
	return normalized, hasAnchor && len(normalized) > 0
}

func normalizeReplayItem(item map[string]interface{}) (map[string]interface{}, bool) {
	switch strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])) {
	case "reasoning":
		return normalizeReasoningReplayItem(item)
	case "message":
		return normalizeAssistantMessageReplayItem(item)
	case "function_call":
		return normalizeFunctionCallReplayItem(item)
	case "custom_tool_call":
		return normalizeCustomToolCallReplayItem(item)
	default:
		return nil, false
	}
}

func normalizeReasoningReplayItem(item map[string]interface{}) (map[string]interface{}, bool) {
	encrypted, ok := item["encrypted_content"].(string)
	if !ok || !validReplayCipher(encrypted) {
		return nil, false
	}
	// xAI treats extra keys such as content:null as a modified compaction blob
	// and 400s, so replay only the portable input shape.
	return map[string]interface{}{
		"type":              "reasoning",
		"summary":           []interface{}{},
		"encrypted_content": encrypted,
	}, true
}

func normalizeAssistantMessageReplayItem(item map[string]interface{}) (map[string]interface{}, bool) {
	if !strings.EqualFold(strings.TrimSpace(chatwire.ParseLooseStringAny(item["role"])), "assistant") {
		return nil, false
	}
	content, ok := item["content"].([]interface{})
	if !ok || len(content) == 0 {
		return nil, false
	}
	parts := make([]interface{}, 0, len(content))
	for _, entry := range content {
		part, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(part["type"])) {
		case "output_text":
			text, ok := part["text"].(string)
			if !ok {
				continue
			}
			parts = append(parts, map[string]interface{}{"type": "output_text", "text": text})
		case "refusal":
			refusal, ok := part["refusal"].(string)
			if !ok {
				continue
			}
			parts = append(parts, map[string]interface{}{"type": "refusal", "refusal": refusal})
		}
	}
	if len(parts) == 0 {
		return nil, false
	}
	return map[string]interface{}{"type": "message", "role": "assistant", "content": parts}, true
}

func normalizeFunctionCallReplayItem(item map[string]interface{}) (map[string]interface{}, bool) {
	callID := strings.TrimSpace(chatwire.ParseLooseStringAny(item["call_id"]))
	name := strings.TrimSpace(chatwire.ParseLooseStringAny(item["name"]))
	arguments, ok := item["arguments"].(string)
	if !ok || callID == "" || name == "" {
		return nil, false
	}
	return map[string]interface{}{"type": "function_call", "call_id": callID, "name": name, "arguments": arguments}, true
}

func normalizeCustomToolCallReplayItem(item map[string]interface{}) (map[string]interface{}, bool) {
	callID := strings.TrimSpace(chatwire.ParseLooseStringAny(item["call_id"]))
	name := strings.TrimSpace(chatwire.ParseLooseStringAny(item["name"]))
	input, exists := item["input"]
	if !exists || callID == "" || name == "" {
		return nil, false
	}
	status := strings.TrimSpace(chatwire.ParseLooseStringAny(item["status"]))
	status = util.FirstNonEmptyUntrimmed(status, "completed")
	return map[string]interface{}{"type": "custom_tool_call", "status": status, "call_id": callID, "name": name, "input": input}, true
}

// comparableReplayCallIDs returns the equivalent spellings of a tool call ID.
// Anthropic-style clients prefix the same upstream call with "toolu_".
func comparableReplayCallIDs(callID string) []string {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return nil
	}
	const anthropicPrefix = "toolu_"
	if strings.HasPrefix(callID, anthropicPrefix) {
		if upstreamID := strings.TrimPrefix(callID, anthropicPrefix); upstreamID != "" {
			return []string{callID, upstreamID}
		}
		return []string{callID}
	}
	return []string{callID, anthropicPrefix + callID}
}

func replayToolCallKeys(itemType, callID string) []string {
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return nil
	}
	ids := comparableReplayCallIDs(callID)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, itemType+replayToolCallSeparator+id)
	}
	return keys
}

func anyReplayCallKeyExists(existing map[string]bool, keys []string) bool {
	return slices.ContainsFunc(keys, func(key string) bool { return existing[key] })
}

// cloneReplayItemWithCallID rebinds a replayed tool call to the spelling the
// client's own tool output already uses, so the pair stays matched.
func cloneReplayItemWithCallID(item map[string]interface{}, callID string) map[string]interface{} {
	clone := make(map[string]interface{}, len(item))
	for key, value := range item {
		clone[key] = value
	}
	clone["call_id"] = callID
	return clone
}

func lastAssistantReplayMessage(input []interface{}) (map[string]interface{}, bool) {
	for index := len(input) - 1; index >= 0; index-- {
		item, ok := input[index].(map[string]interface{})
		if !ok {
			continue
		}
		typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
		if (typeName != "" && typeName != "message") || !strings.EqualFold(strings.TrimSpace(chatwire.ParseLooseStringAny(item["role"])), "assistant") {
			continue
		}
		return item, true
	}
	return nil, false
}

func cachedAssistantReplayMessage(items []interface{}) (map[string]interface{}, bool) {
	for _, entry := range items {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])) == "message" && strings.EqualFold(strings.TrimSpace(chatwire.ParseLooseStringAny(item["role"])), "assistant") {
			return item, true
		}
	}
	return nil, false
}

type replayAssistantPart struct {
	partType string
	value    string
}

func replayAssistantParts(raw interface{}) ([]replayAssistantPart, bool) {
	if raw == nil {
		return nil, false
	}
	if text, ok := raw.(string); ok {
		return []replayAssistantPart{{partType: "output_text", value: text}}, true
	}
	parts, ok := raw.([]interface{})
	if !ok {
		return nil, false
	}
	result := make([]replayAssistantPart, 0, len(parts))
	for _, entry := range parts {
		part, ok := entry.(map[string]interface{})
		if !ok {
			return nil, false
		}
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(part["type"])) {
		case "output_text":
			text, ok := part["text"].(string)
			if !ok {
				return nil, false
			}
			result = append(result, replayAssistantPart{partType: "output_text", value: text})
		case "refusal":
			refusal, ok := part["refusal"].(string)
			if !ok {
				return nil, false
			}
			result = append(result, replayAssistantPart{partType: "refusal", value: refusal})
		default:
			return nil, false
		}
	}
	return result, len(result) > 0
}

func replayAssistantContentEqual(left, right map[string]interface{}) bool {
	leftParts, leftOK := replayAssistantParts(left["content"])
	rightParts, rightOK := replayAssistantParts(right["content"])
	if !leftOK || !rightOK || len(leftParts) != len(rightParts) {
		return false
	}
	for i := range leftParts {
		if leftParts[i] != rightParts[i] {
			return false
		}
	}
	return true
}

// filterReplayItemsForInput drops anything the request already carries, so a
// replay never duplicates the client's own history. It returns nil when the
// client's last assistant message disagrees with the cached one, because then
// the cache belongs to a different branch of the conversation.
func filterReplayItemsForInput(input []interface{}, items []interface{}) []interface{} {
	if len(input) == 0 {
		return nil
	}
	lastAssistant, hasLastAssistant := lastAssistantReplayMessage(input)
	cachedAssistant, hasCachedAssistant := cachedAssistantReplayMessage(items)
	assistantMatches := hasLastAssistant && hasCachedAssistant && replayAssistantContentEqual(lastAssistant, cachedAssistant)
	if hasLastAssistant && hasCachedAssistant && !assistantMatches {
		return nil
	}
	existingCalls := map[string]bool{}
	existingOutputs := map[string]string{}
	existingEncrypted := map[string]bool{}
	for _, entry := range input {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])) {
		case "reasoning":
			if encrypted := strings.TrimSpace(chatwire.ParseLooseStringAny(item["encrypted_content"])); encrypted != "" {
				existingEncrypted[encrypted] = true
			}
		case "function_call_output", "custom_tool_call_output":
			callID := chatwire.ParseLooseStringAny(item["call_id"])
			for _, candidate := range comparableReplayCallIDs(callID) {
				existingOutputs[candidate] = callID
			}
		case "function_call", "custom_tool_call":
			callID := chatwire.ParseLooseStringAny(item["call_id"])
			for _, key := range replayToolCallKeys(strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])), callID) {
				existingCalls[key] = true
			}
		}
	}
	filtered := make([]interface{}, 0, len(items))
	for _, entry := range items {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
		switch typeName {
		case "reasoning":
			if existingEncrypted[strings.TrimSpace(chatwire.ParseLooseStringAny(item["encrypted_content"]))] {
				continue
			}
		case "message":
			if assistantMatches {
				continue
			}
		case "function_call", "custom_tool_call":
			callID := chatwire.ParseLooseStringAny(item["call_id"])
			keys := replayToolCallKeys(typeName, callID)
			if len(keys) == 0 || anyReplayCallKeyExists(existingCalls, keys) {
				continue
			}
			// A tool call is only replayable when its output is already present;
			// otherwise the turn would be re-issued instead of continued.
			outputCallID := ""
			for _, candidate := range comparableReplayCallIDs(callID) {
				if value := existingOutputs[candidate]; value != "" {
					outputCallID = value
					break
				}
			}
			if outputCallID == "" {
				continue
			}
			for _, key := range keys {
				existingCalls[key] = true
			}
			if outputCallID != callID {
				item = cloneReplayItemWithCallID(item, outputCallID)
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// insertReplayItems places the replay immediately before the point it belongs:
// ahead of the matching tool output, else ahead of the last assistant message,
// else ahead of the first non-system input.
func insertReplayItems(input []interface{}, items []interface{}) []interface{} {
	if len(items) == 0 {
		return input
	}
	insertAt := replayInsertIndex(input, items)
	next := make([]interface{}, 0, len(input)+len(items))
	next = append(next, input[:insertAt]...)
	next = append(next, items...)
	next = append(next, input[insertAt:]...)
	return next
}

func replayInsertIndex(input []interface{}, items []interface{}) int {
	replayCallIDs := map[string]bool{}
	for _, entry := range items {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
		if typeName == "function_call" || typeName == "custom_tool_call" {
			for _, id := range comparableReplayCallIDs(chatwire.ParseLooseStringAny(item["call_id"])) {
				replayCallIDs[id] = true
			}
		}
	}
	if len(replayCallIDs) > 0 {
		for index, entry := range input {
			item, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
			if typeName != "function_call_output" && typeName != "custom_tool_call_output" {
				continue
			}
			callIDs := comparableReplayCallIDs(chatwire.ParseLooseStringAny(item["call_id"]))
			if len(callIDs) == 0 {
				return index
			}
			for _, callID := range callIDs {
				if replayCallIDs[callID] {
					return index
				}
			}
		}
	}
	for index := len(input) - 1; index >= 0; index-- {
		item, ok := input[index].(map[string]interface{})
		if !ok {
			continue
		}
		typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
		if (typeName == "" || typeName == "message") && strings.EqualFold(strings.TrimSpace(chatwire.ParseLooseStringAny(item["role"])), "assistant") {
			return index
		}
	}
	for index, entry := range input {
		if shouldInsertReplayBefore(entry) {
			return index
		}
	}
	return len(input)
}

func shouldInsertReplayBefore(entry interface{}) bool {
	item, ok := entry.(map[string]interface{})
	if !ok {
		return true
	}
	typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
	role := strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(item["role"])))
	if role == "" || (typeName != "" && typeName != "message") {
		return true
	}
	return role != "system" && role != "developer"
}

// reasoningForCalls indexes the cached turn's reasoning items by the call id of
// the function_call that follows them. A multi-turn tool loop only needs the
// reasoning proof that belongs to the call it is answering, and a client that
// echoes its own calls may drop that block.
func reasoningForCalls(items []interface{}) map[string]map[string]interface{} {
	index := map[string]map[string]interface{}{}
	var pending map[string]interface{}
	for _, entry := range items {
		item, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])) {
		case "reasoning":
			if encrypted := strings.TrimSpace(chatwire.ParseLooseStringAny(item["encrypted_content"])); encrypted != "" {
				pending = item
			}
		case "function_call", "custom_tool_call":
			if pending == nil {
				continue
			}
			callID := strings.TrimSpace(chatwire.ParseLooseStringAny(item["call_id"]))
			if callID == "" {
				continue
			}
			for _, key := range comparableReplayCallIDs(callID) {
				index[key] = pending
			}
		}
	}
	return index
}

// hasReasoningBefore reports whether a reasoning item precedes position in the
// input, which is what the upstream requires for a call to carry its proof.
func hasReasoningBefore(input []interface{}, position int) bool {
	for i := position - 1; i >= 0; i-- {
		item, ok := input[i].(map[string]interface{})
		if !ok {
			return false
		}
		switch strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])) {
		case "reasoning":
			return strings.TrimSpace(chatwire.ParseLooseStringAny(item["encrypted_content"])) != ""
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output", "message":
			// A call boundary was reached without seeing the proof.
			return false
		}
	}
	return false
}

// backfillReasoningForCalls inserts the reasoning item that belongs to a
// function_call the client echoed without its proof.
//
// This is the per-call counterpart of the whole-turn replay: a client that
// resends only part of its history (or reorders it) still gets the proof the
// upstream needs, instead of the upstream silently losing the reasoning chain.
func backfillReasoningForCalls(input []interface{}, cached []interface{}) []interface{} {
	if len(input) == 0 || len(cached) == 0 {
		return input
	}
	index := reasoningForCalls(cached)
	if len(index) == 0 {
		return input
	}
	seen := map[string]bool{}
	added := 0
	out := make([]interface{}, 0, len(input))
	for position, entry := range input {
		item, ok := entry.(map[string]interface{})
		if !ok {
			out = append(out, entry)
			continue
		}
		typeName := strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"]))
		if typeName == "function_call" || typeName == "custom_tool_call" {
			callID := strings.TrimSpace(chatwire.ParseLooseStringAny(item["call_id"]))
			for _, key := range comparableReplayCallIDs(callID) {
				proof, exists := index[key]
				if !exists || seen[key] || hasReasoningBefore(append(out, entry), position) {
					continue
				}
				seen[key] = true
				out = append(out, cloneReplayItems([]interface{}{proof})...)
				added++
				break
			}
		}
		out = append(out, entry)
	}
	if added == 0 {
		return input
	}
	return out
}
