package grok

import (
	"fmt"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"

	"orchids-api/internal/modelpolicy"
)

// applyBuildResponseDefaults applies two defaults to every Build request:
// `store` defaults to false (ZDR) and `include` always asks for
// reasoning.encrypted_content. An explicit `store` from the caller is kept, and
// an existing include list keeps its other entries and its order.
func applyBuildResponseDefaults(payload map[string]interface{}) {
	if payload == nil {
		return
	}
	if raw, exists := payload["store"]; !exists || raw == nil {
		payload["store"] = false
	}
	const reasoningInclude = "reasoning.encrypted_content"
	includes := make([]interface{}, 0, 2)
	switch typed := payload["include"].(type) {
	case []interface{}:
		includes = append(includes, typed...)
	case []string:
		for _, value := range typed {
			includes = append(includes, value)
		}
	case nil:
	default:
		includes = append(includes, typed)
	}
	for _, value := range includes {
		if text, ok := value.(string); ok && strings.TrimSpace(text) == reasoningInclude {
			payload["include"] = includes
			return
		}
	}
	payload["include"] = append(includes, reasoningInclude)
}

func hasBuildHostedTool(tools []map[string]interface{}, kind string) bool {
	for _, tool := range tools {
		if strings.EqualFold(strings.TrimSpace(chatwire.ParseLooseStringAny(tool["type"])), kind) {
			return true
		}
	}
	return false
}

func normalizeBuildResponsesPayload(payload map[string]interface{}) error {
	if err := responses.ValidateToolsAndHistory(payload); err != nil {
		return err
	}
	// The two defaults applied to every Build request. They are the
	// reason a Codex turn can build a reasoning-replay chain at all: without the
	// include the upstream never returns encrypted_content, and `store:false` is
	// the zero-data-retention default the reference implementation documents.
	applyBuildResponseDefaults(payload)
	if raw, ok := payload["response_format"].(map[string]interface{}); ok {
		delete(payload, "response_format")
		if _, exists := payload["text"]; !exists {
			payload["text"] = map[string]interface{}{"format": normalizeChatResponseFormat(raw)}
		}
	}
	tools := responses.InterfaceMaps(payload["tools"])
	if len(tools) == 0 {
		delete(payload, "tools")
		delete(payload, "tool_choice")
		return nil
	}
	// Build's cache-capable hosted-search route expects x_search to accompany a
	// web_search declaration. The official Build adapter adds this internal
	// routing tool; forwarding web_search alone
	// makes the model begin the search and then terminate with upstream_rejection.
	// Keep an explicit x_search unchanged and never add web_search when only X was
	// requested, so this does not broaden a caller's search permission.
	if hasBuildHostedTool(tools, "web_search") && !hasBuildHostedTool(tools, "x_search") {
		tools = append(tools, map[string]interface{}{"type": "x_search"})
	}
	payload["tools"] = tools
	return responses.ValidateToolsAndHistory(payload)
}

// Chat Completions has no native reasoning object. Accept the relay's
// reasoning_effort / reasoning_summary extensions and rebuild the Responses
// shape from them. A control the caller omitted stays omitted rather than being
// guessed here; plane-specific defaults are applied later.
func chatReasoningControls(req *chatwire.Request) map[string]interface{} {
	if req == nil {
		return nil
	}
	reasoning := map[string]interface{}{}
	if req.ReasoningEffort != nil {
		if effort := strings.TrimSpace(*req.ReasoningEffort); effort != "" {
			reasoning["effort"] = effort
		}
	}
	if req.ReasoningSummary != nil {
		if summary := strings.TrimSpace(*req.ReasoningSummary); summary != "" {
			reasoning["summary"] = summary
		}
	}
	return reasoning
}

// normalizeBuildReasoningEffort maps client effort aliases onto levels the
// selected model actually accepts. Grok 4.5 and other models without an xhigh
// wire contract take the proven defensive xhigh/max -> high mapping; models
// that do advertise xhigh keep it. Composer never receives an effort at all,
// but keeps its other reasoning controls such as summary.
func normalizeBuildReasoningEffort(payload map[string]interface{}, model string) {
	reasoning, _ := payload["reasoning"].(map[string]interface{})
	if reasoning == nil {
		return
	}
	effort := strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(reasoning["effort"])))
	if effort == "" {
		return
	}
	if modelpolicy.IsGrokComposerModel(model) {
		delete(reasoning, "effort")
		if len(reasoning) == 0 {
			delete(payload, "reasoning")
		}
		return
	}
	var normalized string
	switch effort {
	case "minimal":
		normalized = "low"
	case "xhigh", "max":
		if modelpolicy.SupportsReasoningEffort(model, "xhigh") {
			normalized = "xhigh"
		} else {
			normalized = "high"
		}
	default:
		return
	}
	reasoning["effort"] = normalized
}

func validatePayloadReasoning(payload map[string]interface{}) error {
	if raw, exists := payload["reasoning"]; exists && raw != nil {
		reasoning, ok := raw.(map[string]interface{})
		if !ok {
			return fmt.Errorf("reasoning must be an object")
		}
		if raw, exists := reasoning["effort"]; exists {
			if effort, ok := raw.(string); !ok || strings.TrimSpace(effort) == "" {
				return fmt.Errorf("reasoning.effort must be a non-empty string")
			}
		}
		if raw, exists := reasoning["summary"]; exists {
			if summary, ok := raw.(string); !ok || strings.TrimSpace(summary) == "" {
				return fmt.Errorf("reasoning.summary must be a non-empty string")
			}
		}
	}
	return nil
}

// hasNativeSearchTool reports whether the tool list already declares a hosted
// search tool, so web_search_options does not duplicate it.
func hasNativeSearchTool(tools []map[string]interface{}) bool {
	return hasBuildHostedTool(tools, "web_search") || hasBuildHostedTool(tools, "x_search")
}
