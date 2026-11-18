package responses

import "orchids-api/internal/adapter"

// UsageFromChat projects inclusive Chat or exclusive native Messages counters
// without turning missing usage into zero. Cache writes remain separately
// observable, but are included only once in input and total counters.
func UsageFromChat(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	if len(usage) == 0 {
		return nil
	}
	usage = adapter.ProjectChatUsage(usage)
	input, hasInput := usageCounter(usage["prompt_tokens"])
	output, hasOutput := usageCounter(usage["completion_tokens"])
	total, hasTotal := usageCounter(usage["total_tokens"])
	result := CloneStringInterfaceMap(usage)
	for _, key := range []string{"prompt_tokens", "completion_tokens", "prompt_tokens_details", "completion_tokens_details", "input_tokens", "output_tokens", "total_tokens"} {
		delete(result, key)
	}
	if hasInput {
		result["input_tokens"] = input
	}
	if hasOutput {
		result["output_tokens"] = output
	}
	if hasTotal {
		result["total_tokens"] = total
	} else if hasInput && hasOutput {
		if total, valid := adapter.AddUsageCounters(input, output); valid {
			result["total_tokens"] = total
		}
	}
	for target, source := range map[string]string{"input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
		if details, ok := FirstNonNil(usage[target], usage[source]).(map[string]any); ok {
			result[target] = CloneStringInterfaceMap(details)
		}
	}
	if cached, valid := usageCounter(usage["cache_read_input_tokens"]); valid {
		details, _ := result["input_tokens_details"].(map[string]any)
		if details == nil {
			details = make(map[string]any, 1)
			result["input_tokens_details"] = details
		}
		if _, exists := details["cached_tokens"]; !exists {
			details["cached_tokens"] = cached
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func usageCounter(value any) (int, bool) {
	return adapter.UsageCounter(value)
}
