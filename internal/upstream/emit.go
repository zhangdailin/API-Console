package upstream

import "orchids-api/internal/util"

// reasoningUsage resolves the reasoning-token count of an upstream usage
// object, reading every alias the gateways have shipped:
//
//   - the OpenAI-style top-level key: reasoning_tokens / reasoningTokens
//   - GLM-compatible gateways: completion_thinking_tokens
//   - the nested OpenAI details object: completion_tokens_details /
//     completionTokensDetails / output_tokens_details / outputTokensDetails
func reasoningUsage(raw map[string]interface{}) (int, bool) {
	if value, ok := util.UsageInt(raw, "reasoning_tokens", "reasoningTokens", "completion_thinking_tokens"); ok {
		return value, true
	}
	for _, key := range []string{"completion_tokens_details", "completionTokensDetails", "output_tokens_details", "outputTokensDetails"} {
		if details := util.UsageMap(raw, key); details != nil {
			if value, found := util.UsageInt(details, "reasoning_tokens", "reasoningTokens", "thinking_tokens", "thinkingTokens"); found {
				return value, true
			}
		}
	}
	return 0, false
}

// The emitters below are shared by every provider stream reader.
//
// Each reader reports the same three things — a text delta, a completed tool
// call and a usage payload — through the same internal event shapes. The three
// copies of that code were drifting, so one implementation lives here. Nothing
// below is provider specific.
//
// The counters are passed by pointer because each reader keeps its own
// streamResult type: the helpers only touch the fields every one of them has
// (SawMeaningfulEvent, ToolCallCount, Usage).

// EmitTextDelta reports one assistant text delta and marks the stream
// meaningful. An empty delta is dropped, because the splitters that feed this
// call emit empty strings whenever a frame carried no text.
func EmitTextDelta(onMessage func(SSEMessage), text string, saw *bool) {
	if text == "" {
		return
	}
	if saw != nil {
		*saw = true
	}
	if onMessage != nil {
		onMessage(SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{"delta": text}})
	}
}

// EmitToolCalls reports every accumulated tool call and counts them.
//
// Arguments can span several deltas, so a call is only delivered once the
// accumulator has closed it; draining is what makes a double flush safe.
func EmitToolCalls(onMessage func(SSEMessage), states []*util.ToolCall, saw *bool, count *int) {
	for _, state := range states {
		if saw != nil {
			*saw = true
		}
		if count != nil {
			*count++
		}
		if onMessage == nil {
			continue
		}
		id := state.ID
		if id == "" {
			id = util.NewToolCallID()
		}
		onMessage(SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{
			"toolCallId": id,
			"toolName":   state.Name,
			"input":      state.Arguments,
		}})
	}
}

// ApplyStreamUsage stores a normalized usage payload on the stream result and
// reports the shared usage event. A nil or empty payload is a no-op, so a
// caller can hand over whatever its own normalizer produced.
func ApplyStreamUsage(onMessage func(SSEMessage), usage map[string]interface{}, saw *bool, dst *map[string]interface{}) {
	if len(usage) == 0 {
		return
	}
	if dst != nil {
		*dst = usage
	}
	if saw != nil {
		*saw = true
	}
	if onMessage != nil {
		onMessage(SSEMessage{Type: "model.tokens-used", Event: usage})
	}
}

// NormalizeUsageMap maps an upstream usage object onto the key pairs the shared
// stream handler consumes. Both spellings of each key are emitted because the
// handler reads either one and prefers the camelCase form.
//
// Reasoning is read from every alias the gateways have shipped: a bare
// reasoning count at the top level, the GLM-style completion_thinking_tokens,
// and the OpenAI-style nested details object.
func NormalizeUsageMap(raw map[string]interface{}) map[string]interface{} {
	if len(raw) == 0 {
		return nil
	}
	input, hasInput := util.UsageInt(raw, "prompt_tokens", "promptTokens", "input_tokens", "inputTokens")
	output, hasOutput := util.UsageInt(raw, "completion_tokens", "completionTokens", "output_tokens", "outputTokens")
	if !hasInput && !hasOutput {
		return nil
	}
	out := make(map[string]interface{}, 6)
	if hasInput {
		out["inputTokens"] = input
		out["input_tokens"] = input
	}
	if hasOutput {
		out["outputTokens"] = output
		out["output_tokens"] = output
	}
	if cached, ok := util.UsageInt(raw, "prompt_cache_hit_tokens", "cached_tokens"); ok {
		out["cacheReadTokens"] = cached
		out["cache_read_tokens"] = cached
	}
	if reasoning, ok := reasoningUsage(raw); ok {
		out["reasoningTokens"] = reasoning
		out["reasoning_tokens"] = reasoning
	}
	return out
}
