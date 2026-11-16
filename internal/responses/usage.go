package responses

// UsageFromChat renames reported counters without turning missing usage into
// zero. A total can be derived only when both input and output are known.
func UsageFromChat(raw any) map[string]any {
	usage, _ := raw.(map[string]any)
	if len(usage) == 0 {
		return nil
	}
	input, hasInput := usageCounter(FirstNonNil(usage["prompt_tokens"], usage["input_tokens"]))
	output, hasOutput := usageCounter(FirstNonNil(usage["completion_tokens"], usage["output_tokens"]))
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
		result["total_tokens"] = input + output
	}
	for target, source := range map[string]string{"input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
		if details, ok := FirstNonNil(usage[target], usage[source]).(map[string]any); ok {
			result[target] = CloneStringInterfaceMap(details)
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func usageCounter(value any) (int, bool) {
	counter, ok := nonNegativeJSONIntegerOK(value)
	if !ok || int64(int(counter)) != counter {
		return 0, false
	}
	return int(counter), true
}
