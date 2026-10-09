package handler

import (
	"encoding/json"
)

// addOutputTokens folds a fragment into the running output estimate.
//
// Token accounting lives under h.mu rather than a mutex of its own: resetRoundState
// already cleared outputTokens and the estimator under h.mu, so a second mutex
// gave no real exclusion over the same fields. One lock for the handler's state
// is what lets the per-token delta paths take the lock once per frame.
func (h *streamHandler) addOutputTokens(text string) {
	if text == "" {
		return
	}
	h.mu.Lock()
	if !h.useUpstreamUsage {
		h.outputEstimator.Add(text)
	}
	h.mu.Unlock()
}

func (h *streamHandler) finalizeOutputTokens() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.useUpstreamUsage {
		return
	}
	h.outputTokens = h.outputEstimator.Count()
}

// applyUpstreamUsageTokens folds one provider usage payload into the reported
// totals under one lock. Omitted fields keep their local estimates; metadata
// alone still counts as provider usage evidence.
func (h *streamHandler) applyUpstreamUsageTokens(usage map[string]interface{}) {
	if len(usage) == 0 {
		return
	}
	read := func(keys ...string) (int, bool) {
		for _, key := range keys {
			if value, ok := getUsageIntValue(usage, key); ok {
				return value, true
			}
		}
		return 0, false
	}
	input, hasInput := read("inputTokens", "input_tokens")
	output, hasOutput := read("outputTokens", "output_tokens")
	cached, hasCached := read("cacheReadTokens", "cache_read_tokens")
	cacheWrite, hasCacheWrite := read("cacheWriteTokens", "cache_creation_input_tokens")
	reasoning, hasReasoning := read("reasoningTokens", "reasoning_tokens")
	_, hasCredits := usage["credits"]
	_, hasOriginalCredits := usage["original_credits"]
	metadataKeys := []string{"billable", "cacheable_tokens", "firstTokenDuration", "totalDuration", "serverDuration"}
	hasMetadata := false
	for _, key := range metadataKeys {
		if _, ok := usage[key]; ok {
			hasMetadata = true
		}
	}
	if !hasInput && !hasOutput && !hasCached && !hasCacheWrite && !hasReasoning && !hasCredits && !hasOriginalCredits && !hasMetadata {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if hasInput || hasOutput || hasCached || hasCacheWrite || hasReasoning || hasCredits || hasOriginalCredits {
		h.useUpstreamUsage = true
	}
	if hasInput {
		h.inputTokens = input
	}
	if hasOutput {
		h.outputTokens = output
	}
	if hasCached {
		h.cachedInputTokens = cached
	}
	if hasCacheWrite {
		h.cacheWriteTokens = cacheWrite
	}
	if hasReasoning {
		h.reasoningTokens = reasoning
	}
	if hasCredits || hasOriginalCredits || hasMetadata {
		if h.usageMetadata == nil {
			h.usageMetadata = make(map[string]interface{}, 7)
		}
		for _, key := range metadataKeys {
			if value, ok := usage[key]; ok {
				h.usageMetadata[key] = value
			}
		}
		if hasCredits {
			h.usageMetadata["credits"] = usage["credits"]
		}
		if hasOriginalCredits {
			h.usageMetadata["original_credits"] = usage["original_credits"]
		}
	}
}

func getUsageIntValue(usage map[string]interface{}, key string) (int, bool) {
	value, ok := usage[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		n, err := typed.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func (h *streamHandler) setUsageTokens(input, output int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.useUpstreamUsage = true
	if input >= 0 {
		h.inputTokens = input
	}
	if output >= 0 {
		h.outputTokens = output
	}
}
