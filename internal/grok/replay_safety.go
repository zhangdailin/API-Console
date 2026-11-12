package grok

import (
	"strings"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/util"
)

// requestReplayUnsafe reports whether replaying this request on another
// account could duplicate an external side effect: a hosted tool may already
// have run, so a failure is not retried elsewhere.
func requestReplayUnsafe(req *chatwire.Request) bool {
	if req == nil {
		return false
	}
	if req.WebSearchOptions != nil || len(req.MCPServers) > 0 {
		return true
	}
	for _, tool := range req.ResponsesTools {
		switch strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(tool["type"]))) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			// These only ask the model to return a call; the client runs it.
			continue
		case "shell":
			environment, _ := tool["environment"].(map[string]any)
			if strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(environment["type"]))) != "local" {
				return true
			}
		default:
			// Every server-side tool type, including ones this gateway does not
			// know yet, defaults to "do not replay".
			return true
		}
	}
	for _, tool := range req.Tools {
		declaration := tool.Raw
		if declaration == nil {
			declaration = map[string]any{"type": tool.Type}
		}
		switch strings.ToLower(strings.TrimSpace(util.FirstNonEmpty(tool.Type, chatwire.ParseLooseStringAny(declaration["type"])))) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			continue
		case "shell":
			environment, _ := declaration["environment"].(map[string]any)
			if strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(environment["type"]))) != "local" {
				return true
			}
		default:
			return true
		}
	}
	return false
}
