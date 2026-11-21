package grok

import (
	"net/http"
	"orchids-api/internal/chatwire"
)

// recoverChatReasoningWithinBudget runs only after a recognized pre-generation decode 400.
// A genuine compaction rejection is client-held upstream state and is never
// rewritten; every other recognized decode failure is recoverable.
func recoverChatReasoningWithinBudget(payload map[string]any, response *http.Response, initial error, canCall func() bool, call func(string) (*http.Response, error)) (*http.Response, error) {
	if preservesClientCompaction(payload, initial) || !canCall() {
		return response, initial
	}
	failure := initial
	if stripInjectedReasoningReplay(payload) {
		response, failure = call("reasoning_encrypted_content_retry")
		if failure == nil || !isReasoningReplayDecodeError(failure) {
			return response, failure
		}
	}
	if !canCall() || chatwire.ParseLooseStringAny(payload["previous_response_id"]) != "" || chatwire.ParseLooseStringAny(payload["prompt_cache_key"]) == "" {
		return response, failure
	}
	delete(payload, "prompt_cache_key")
	return call("reasoning_session_reset")
}
