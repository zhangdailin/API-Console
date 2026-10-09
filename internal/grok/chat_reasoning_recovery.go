package grok

import (
	"net/http"
	"orchids-api/internal/chatwire"
)

// recoverChatReasoning runs only after a recognized pre-generation decode 400.
// A genuine compaction rejection is client-held upstream state and is never
// rewritten; every other recognized decode failure is recoverable.
func recoverChatReasoning(payload map[string]interface{}, initial error, call func(string) (*http.Response, error)) (*http.Response, error) {
	if preservesClientCompaction(payload, initial) {
		return nil, initial
	}
	var response *http.Response
	failure := initial
	if stripInjectedReasoningReplay(payload) {
		response, failure = call("reasoning_encrypted_content_retry")
		if failure == nil || !isReasoningReplayDecodeError(failure) {
			return response, failure
		}
	}
	if chatwire.ParseLooseStringAny(payload["previous_response_id"]) != "" || chatwire.ParseLooseStringAny(payload["prompt_cache_key"]) == "" {
		return response, failure
	}
	delete(payload, "prompt_cache_key")
	return call("reasoning_session_reset")
}
