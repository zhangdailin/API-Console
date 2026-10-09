package handler

import (
	"log/slog"
	"slices"
	"strings"

	"encoding/json"

	"orchids-api/internal/util"
)

func (h *streamHandler) setDisallowToolCalls(disallow bool) {
	h.mu.Lock()
	h.disallowToolCalls = disallow
	h.mu.Unlock()
}

func (h *streamHandler) setAllowedToolNames(names []string) {
	h.mu.Lock()
	clear(h.allowedToolNames)
	for _, name := range names {
		key := strings.TrimSpace(name)
		if key == "" {
			continue
		}
		h.allowedToolNames[key] = struct{}{}
	}
	h.mu.Unlock()
}

func (h *streamHandler) setSurfaceToolRejects(surface bool) {
	h.mu.Lock()
	h.surfaceToolRejects = surface
	h.mu.Unlock()
}

func (h *streamHandler) emitToolCallNonStream(call toolCall) {
	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	inputJSON := strings.TrimSpace(call.input)
	inputJSON = util.FirstNonEmptyUntrimmed(inputJSON, "{}")
	var inputValue interface{}
	if err := json.Unmarshal([]byte(inputJSON), &inputValue); err != nil {
		inputValue = map[string]interface{}{}
	}
	h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
		"type":  "tool_use",
		"id":    call.id,
		"name":  call.name,
		"input": inputValue,
	})
}

func (h *streamHandler) emitToolCallStream(call toolCall, idx int, final bool) {
	if call.id == "" {
		return
	}

	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	inputJSON := strings.TrimSpace(call.input)
	inputJSON = util.FirstNonEmptyUntrimmed(inputJSON, "{}")

	h.mu.Lock()
	defer h.mu.Unlock()
	if idx < 0 {
		h.blockIndex++
		idx = h.blockIndex
	}
	h.writeSSEContentBlockStartToolUseLocked(idx, call.id, call.name, final)
	h.writeSSEContentBlockDeltaInputJSONLocked(idx, inputJSON, final)
	h.writeSSEContentBlockStopLocked(idx, final)
}

// emitToolUseFromInput emits a single tool_use block once the full input is available.
func (h *streamHandler) emitToolUseFromInput(toolID, toolName, inputStr string) {
	if toolID == "" || toolName == "" {
		return
	}
	if _, ok := h.toolCallEmitted[toolID]; ok {
		return
	}
	h.toolCallEmitted[toolID] = struct{}{}

	h.addOutputTokens(toolName)
	inputJSON := strings.TrimSpace(inputStr)
	inputJSON = util.FirstNonEmptyUntrimmed(inputJSON, "{}")

	h.mu.Lock()
	h.toolCallCount++
	h.blockIndex++
	idx := h.blockIndex
	h.writeSSEContentBlockStartToolUseLocked(idx, toolID, toolName, false)
	h.writeSSEContentBlockDeltaInputJSONLocked(idx, inputJSON, false)
	h.writeSSEContentBlockStopLocked(idx, false)
	h.mu.Unlock()
}

func (h *streamHandler) flushPendingToolCalls() {
	h.mu.Lock()
	calls := slices.Clone(h.pendingToolCalls)
	h.pendingToolCalls = nil
	h.mu.Unlock()

	for _, call := range calls {
		if h.isStream {
			h.emitToolCallStream(call, -1, true)
		} else {
			h.emitToolCallNonStream(call)
		}
	}
}

func (h *streamHandler) handleToolCallAfterChecks(call toolCall) {
	h.mu.Lock()
	h.pendingToolCalls = append(h.pendingToolCalls, call)
	h.toolCallCount++
	h.mu.Unlock()
}

func (h *streamHandler) shouldAcceptToolCall(call toolCall) bool {
	h.mu.Lock()
	disallowToolCalls := h.disallowToolCalls
	allowedTool := true
	if len(h.allowedToolNames) > 0 {
		name := strings.TrimSpace(call.name)
		_, allowedTool = h.allowedToolNames[name]
	}
	if !allowedTool && h.surfaceToolRejects && h.emptyOutputFallback == "" {
		h.emptyOutputFallback = "The upstream model attempted to use a tool that is not available in this request."
	}
	if disallowToolCalls && h.surfaceToolRejects && h.emptyOutputFallback == "" {
		h.emptyOutputFallback = "This request did not provide a compatible tool for the attempted operation."
	}
	if disallowToolCalls {
		h.suppressedToolCalls++
	}
	if !allowedTool {
		h.suppressedToolCalls++
	}
	h.mu.Unlock()
	if disallowToolCalls {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("tool call suppressed by no-tools gate", "tool", call.name, "input", call.input)
		}
		return false
	}
	if !allowedTool {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("tool call suppressed because it is not declared in the current request", "tool", call.name, "input", call.input)
		}
		return false
	}

	if !validToolCallInput(call.name, call.input) {
		h.mu.Lock()
		h.suppressedToolCalls++
		if h.surfaceToolRejects && h.emptyOutputFallback == "" {
			h.emptyOutputFallback = "The upstream model returned an invalid tool request."
		}
		h.mu.Unlock()
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("invalid tool call suppressed", "tool", call.name, "input", call.input)
		}
		return false
	}

	return true
}

// Native function arguments must be a JSON object.
func validToolCallInput(name, input string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(input), &object) == nil && object != nil
}
