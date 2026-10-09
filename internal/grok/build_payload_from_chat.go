package grok

import (
	"fmt"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
	"strings"
)

// responsesPayloadFromChat converts Chat/Messages compatibility input into
// the native Responses wire shape used by Build.
func (h *Handler) responsesPayloadFromChat(spec ModelSpec, req *chatwire.Request, build bool) (map[string]interface{}, error) {
	spec.Upstream = UpstreamCLI
	if err := validateNativeChatContent(req.Messages); err != nil {
		return nil, err
	}
	input, instructions := responsesInputFromChatMessages(req.Messages)
	if len(req.ResponsesInput) > 0 {
		input = append([]interface{}(nil), req.ResponsesInput...)
	}
	if req.ReasoningReplay && strings.TrimSpace(req.PromptCacheKey) != "" {
		if items := h.loadReasoningReplayItems(req.Model, req.PromptCacheKey); len(items) > 0 {
			// Filtering drops anything the caller already sent, so a client that
			// resends its own history is never duplicated.
			if filtered := filterReplayItemsForInput(input, items); len(filtered) > 0 {
				input = backfillReasoningForCalls(insertReplayItems(input, filtered), items)
			}
		}
	}
	if len(input) == 0 && instructions == "" {
		return nil, fmt.Errorf("empty message")
	}
	model := spec.UpstreamModel
	payload := map[string]interface{}{"model": model, "input": input}
	if instructions != "" {
		payload["instructions"] = instructions
	}
	if req.Stream {
		payload["stream"] = true
	}
	if req.Temperature != nil {
		payload["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		payload["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		payload["max_output_tokens"] = *req.MaxTokens
	}
	// Chat Completions has no native reasoning object; rebuild the Responses
	// shape from the relay's reasoning_effort / reasoning_summary extensions.
	if reasoning := chatReasoningControls(req); len(reasoning) > 0 {
		payload["reasoning"] = reasoning
	}
	// Stop sequences are enforced locally (the Build chat/stream writers run a
	// stopFilter over the generated text). Sending them upstream as well makes
	// the upstream truncate the turn, so the matched sequence never reaches the
	// gateway and the client loses the stop_sequence it asked to be told about —
	// and the field is not part of the Build wire contract.

	if value := strings.TrimSpace(req.SafetyIdentifier); value != "" {
		payload["safety_identifier"] = value
	}
	if len(req.ResponseText) > 0 {
		payload["text"] = responses.CloneStringInterfaceMap(req.ResponseText)
	} else if len(req.ResponseFormat) > 0 {
		payload["text"] = map[string]interface{}{"format": normalizeChatResponseFormat(req.ResponseFormat)}
	}
	// Both planes return opaque reasoning only when it is explicitly requested.
	// Without it the replay cache is never populated, so a default (auto) turn
	// would silently lose multi-turn reasoning continuity. The appended entry
	// also means the list can never come back empty.
	payload["include"] = util.UniqueStrings(append(append([]string(nil), req.Include...), "reasoning.encrypted_content"))
	tools := append([]map[string]interface{}(nil), req.ResponsesTools...)
	tools = append(tools, buildToolsFromOpenAI(req.Tools)...)
	// OpenAI's web_search_options has no function form: it means "run the
	// hosted search tool". Lower it to the native tool the Responses planes
	// understand when the caller did not already
	// declare a search tool.
	if len(req.WebSearchOptions) > 0 && !hasNativeSearchTool(tools) {
		tools = append(tools, map[string]interface{}{"type": "web_search"})
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		if choice := buildToolChoiceFromOpenAI(req.ToolChoice); choice != nil {
			payload["tool_choice"] = choice
		}
	}
	// These are forwarded unconditionally: a caller that
	// asks for a service tier or attaches metadata must not have it dropped
	// just because the request declared no tools.
	if req.ParallelToolCalls != nil {
		payload["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if len(req.Metadata) > 0 {
		payload["metadata"] = responses.CloneStringInterfaceMap(req.Metadata)
	}
	if tier := strings.TrimSpace(req.ServiceTier); tier != "" {
		payload["service_tier"] = tier
	}
	if err := validatePayloadReasoning(payload); err != nil {
		return nil, err
	}
	if build {
		// The official Grok Build client always asks its Responses backend for a
		// reasoning summary, even at the model's default effort. Chat Completions
		// has no summary parameter, so make that Build-specific default explicit —
		// without overriding a summary the caller already asked for, and without
		// imposing it on native Responses or Anthropic requests.
		if req.SourceOperation == "" && (req.ReasoningEffort == nil || *req.ReasoningEffort != "none") {
			reasoning, _ := payload["reasoning"].(map[string]interface{})
			if reasoning == nil {
				reasoning = map[string]interface{}{}
			}
			if _, exists := reasoning["summary"]; !exists {
				reasoning["summary"] = "concise"
			}
			payload["reasoning"] = reasoning
		}
		if strings.TrimSpace(req.PromptCacheKey) != "" {
			payload["prompt_cache_key"] = strings.TrimSpace(req.PromptCacheKey)
		}
		if err := normalizeBuildResponsesPayload(payload); err != nil {
			return nil, err
		}
		normalizeBuildReasoningEffort(payload, model)
		return payload, nil
	}
	return payload, nil
}
