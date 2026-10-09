package responses

func UsageFromChat(raw interface{}) map[string]interface{} {
	usage, _ := raw.(map[string]interface{})
	input := InterfaceToInt(FirstNonNil(usage["prompt_tokens"], usage["input_tokens"]))
	output := InterfaceToInt(FirstNonNil(usage["completion_tokens"], usage["output_tokens"]))
	total := InterfaceToInt(usage["total_tokens"])
	if total == 0 {
		total = input + output
	}
	result := CloneStringInterfaceMap(usage)
	if result == nil {
		result = map[string]interface{}{}
	}
	delete(result, "prompt_tokens")
	delete(result, "completion_tokens")
	delete(result, "prompt_tokens_details")
	delete(result, "completion_tokens_details")
	result["input_tokens"] = input
	result["output_tokens"] = output
	result["total_tokens"] = total
	for target, source := range map[string]string{"input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
		if details, ok := FirstNonNil(usage[target], usage[source]).(map[string]interface{}); ok {
			result[target] = CloneStringInterfaceMap(details)
		}
	}
	return result
}
