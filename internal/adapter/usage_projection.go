package adapter

import (
	"encoding/json"
	"math"
)

// UsageCounter accepts only observed, nonnegative integer counters. Floating
// point values outside the exact JSON integer range cannot recover their source.
func UsageCounter(value any) (int, bool) {
	var count int64
	switch value := value.(type) {
	case int:
		return value, value >= 0
	case int64:
		count = value
	case json.Number:
		var err error
		count, err = value.Int64()
		if err != nil {
			return 0, false
		}
	case float64:
		if value < 0 || value > 1<<53-1 || math.IsNaN(value) || math.Trunc(value) != value {
			return 0, false
		}
		count = int64(value)
	default:
		return 0, false
	}
	if count < 0 || int64(int(count)) != count {
		return 0, false
	}
	return int(count), true
}

// AddUsageCounters derives a counter only if its sum is representable.
func AddUsageCounters(left, right int) (int, bool) {
	if left < 0 || right < 0 || left > int(^uint(0)>>1)-right {
		return 0, false
	}
	return left + right, true
}

// ProjectChatUsage converts native Messages usage into inclusive Chat counters.
// The presence of prompt_tokens identifies an already-inclusive OpenAI dialect,
// even when vendor cache fields are also present. Missing fields stay unknown;
// an absent cache component contributes nothing, but an invalid one invalidates
// the derived prompt. The input map is never modified.
func ProjectChatUsage(usage map[string]any) map[string]any {
	if len(usage) == 0 {
		return nil
	}
	result := make(map[string]any, len(usage))
	for key, value := range usage {
		result[key] = value
	}
	for _, key := range []string{"input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens"} {
		delete(result, key)
	}

	inputValue, chatDialect := usage["prompt_tokens"]
	if !chatDialect {
		inputValue = usage["input_tokens"]
	}
	input, hasInput := UsageCounter(inputValue)
	if !chatDialect {
		for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
			if value, exists := usage[key]; exists && hasInput {
				cache, valid := UsageCounter(value)
				if !valid {
					hasInput = false
				} else {
					input, hasInput = AddUsageCounters(input, cache)
				}
			}
		}
	}
	outputValue, hasCompletion := usage["completion_tokens"]
	if !hasCompletion || outputValue == nil {
		outputValue = usage["output_tokens"]
	}
	output, hasOutput := UsageCounter(outputValue)
	if hasInput {
		result["prompt_tokens"] = input
	}
	if hasOutput {
		result["completion_tokens"] = output
	}
	if total, valid := UsageCounter(usage["total_tokens"]); valid {
		result["total_tokens"] = total
	} else if hasInput && hasOutput {
		if total, valid := AddUsageCounters(input, output); valid {
			result["total_tokens"] = total
		}
	}

	// Retain the observed write counter separately. It is part of the prompt,
	// not another component to add to total_tokens.
	var details map[string]any
	if existing, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		details = make(map[string]any, len(existing)+1)
		for key, value := range existing {
			details[key] = value
		}
	}
	for _, key := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if value, exists := usage[key]; exists {
			count, valid := UsageCounter(value)
			if !valid {
				delete(result, key)
				continue
			}
			result[key] = count
			if key == "cache_read_input_tokens" {
				if details == nil {
					details = make(map[string]any, 1)
				}
				if _, exists := details["cached_tokens"]; !exists {
					details["cached_tokens"] = count
				}
			}
		}
	}
	if details != nil {
		result["prompt_tokens_details"] = details
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// appendOpenAIChunkUsage extends the existing finish chunk, not the stream.
func appendOpenAIChunkUsage(chunk []byte, usage map[string]any) []byte {
	if len(usage) == 0 {
		return chunk
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return chunk
	}
	chunk = chunk[:len(chunk)-1]
	chunk = append(chunk, `,"usage":`...)
	chunk = append(chunk, raw...)
	return append(chunk, '}')
}

func messageDeltaChatUsage(data []byte) map[string]any {
	var payload struct {
		Usage map[string]json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Usage) == 0 {
		return nil
	}
	// Decode integer counters without passing through float64. Only known
	// usage fields belong on a Chat chunk; unrelated event metadata stays out.
	usage := make(map[string]any, len(payload.Usage))
	for _, key := range []string{"input_tokens", "output_tokens", "prompt_tokens", "completion_tokens", "total_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if raw, exists := payload.Usage[key]; exists {
			usage[key] = json.Number(string(raw))
		}
	}
	return ProjectChatUsage(usage)
}
