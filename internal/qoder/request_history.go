package qoder

import (
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// buildMessages renders the history. System items become a leading system
// message and are also returned separately for the top-level `system` field,
// because the gateway has been observed to read either.
//
// Tool results must survive as `tool` messages: rewriting them into text breaks
// the assistant/tool pairing and the upstream then rejects the history as
// malformed.
func buildMessages(req upstream.UpstreamRequest) ([]chatMessage, string, error) {
	systemParts := make([]string, 0, len(req.System)+1)
	out := make([]chatMessage, 0, len(req.Messages)+1)

	for _, item := range req.System {
		if text := strings.TrimSpace(item.Text); text != "" {
			systemParts = append(systemParts, text)
		}
	}

	toolCallIDs := map[string]bool{}
	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "developer" {
			// `developer` is the renamed system role; folding it keeps the
			// message instead of dropping it.
			role = "system"
		}
		if role == "system" && msg.Content.IsString() {
			if text := strings.TrimSpace(msg.Content.GetText()); text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}

		if msg.Content.IsString() {
			text := msg.Content.GetText()
			if strings.TrimSpace(text) == "" {
				if role == "assistant" && (strings.TrimSpace(msg.ReasoningContent) != "" || len(msg.ReasoningItem) > 0) {
					out = append(out, chatMessage{Role: role, Reasoning: msg.ReasoningContent, ReasoningItem: msg.ReasoningItem})
				}
				continue
			}
			message := chatMessage{Role: normalRole(role), Content: text}
			if role == "assistant" {
				message.Reasoning = msg.ReasoningContent
				message.ReasoningItem = msg.ReasoningItem
			}
			out = append(out, message)
			continue
		}

		switch role {
		case "assistant":
			if message, ok := convertAssistantMessage(msg, toolCallIDs); ok {
				out = append(out, message)
			}
		default:
			out = append(out, convertBlockMessage(role, msg, toolCallIDs)...)
		}
	}

	systemText := strings.Join(systemParts, "\n")
	if systemText != "" {
		out = append([]chatMessage{{Role: "system", Content: systemText}}, out...)
	}
	if len(out) == 0 {
		text := strings.TrimSpace(req.Prompt)
		if text == "" {
			text = "Hello"
		}
		out = append(out, chatMessage{Role: "user", Content: text})
	}
	return out, systemText, nil
}

// normalRole folds the roles the gateway accepts. An unknown role becomes user
// rather than being forwarded, because the upstream rejects the whole request
// over one unrecognised role.
func normalRole(role string) string {
	switch role {
	case "assistant", "system", "tool":
		return role
	default:
		return "user"
	}
}

// convertAssistantMessage maps text, thinking and tool_use blocks onto one
// assistant message.
func convertAssistantMessage(msg prompt.Message, toolCallIDs map[string]bool) (chatMessage, bool) {
	message := chatMessage{Role: "assistant", Reasoning: msg.ReasoningContent, ReasoningItem: msg.ReasoningItem}
	texts := make([]string, 0, 2)
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				texts = append(texts, block.Text)
			}
		case "thinking":
			if message.Reasoning == "" {
				message.Reasoning = block.Thinking
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" {
				id = NewToolCallID()
			}
			call := chatToolCall{ID: id, Type: "function", Index: block.ToolIndex}
			call.Function.Name = name
			call.Function.Arguments = util.CompactToolInput(block.Input)
			message.ToolCalls = append(message.ToolCalls, call)
			toolCallIDs[id] = true
		}
	}
	message.Content = strings.Join(texts, "\n")
	if message.Content == "" && len(message.ToolCalls) == 0 && message.Reasoning == "" && len(message.ReasoningItem) == 0 {
		return chatMessage{}, false
	}
	return message, true
}

// convertBlockMessage maps user/system blocks, splitting tool_result blocks into
// standalone `tool` messages so the assistant/tool pairing stays intact.
func convertBlockMessage(role string, msg prompt.Message, toolCallIDs map[string]bool) []chatMessage {
	blocks := msg.Content.GetBlocks()
	out := make([]chatMessage, 0, len(blocks))
	pendingParts := make([]chatPart, 0, len(blocks))
	preserveParts := false

	flush := func() {
		if len(pendingParts) == 0 {
			return
		}
		message := chatMessage{Role: normalRole(role)}
		if !preserveParts {
			texts := make([]string, 0, len(pendingParts))
			for _, part := range pendingParts {
				texts = append(texts, part.Text)
			}
			message.Content = strings.Join(texts, "\n")
		} else {
			// Preserve text/image interleaving and per-part cache hints. Transfer ownership
			// of this slice so the next segment cannot overwrite earlier parts.
			message.Contents = pendingParts
		}
		out = append(out, message)
		pendingParts = nil
		preserveParts = false
	}

	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				pendingParts = append(pendingParts, chatPart{Type: "text", Text: block.Text, CacheControl: block.CacheControl})
				if block.CacheControl != nil {
					preserveParts = true
				}
			}
		case "image":
			if url := blockImageURL(block); url != "" {
				pendingParts = append(pendingParts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}, CacheControl: block.CacheControl})
				preserveParts = true
			}
		case "tool_result":
			flush()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" {
				continue
			}
			// A result whose call never appeared in this history would leave a
			// dangling tool message, which the upstream rejects. The pairing is
			// tracked so it can be reported instead of silently dropped.
			if !toolCallIDs[toolID] {
				continue
			}
			delete(toolCallIDs, toolID)
			out = append(out, chatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    util.StringifyToolResult(block.Content),
			})
		}
	}
	flush()
	return out
}

// blockImageURL extracts the image reference from a content block. Both the
// Anthropic `source` shape and a plain URL are accepted; the service forwards
// the reference rather than downloading the image.
func blockImageURL(block prompt.ContentBlock) string {
	if block.Source != nil {
		if url := strings.TrimSpace(block.Source.URL); url != "" {
			return url
		}
		if data := strings.TrimSpace(block.Source.Data); data != "" {
			mediaType := strings.TrimSpace(block.Source.MediaType)
			if mediaType == "" {
				mediaType = "image/png"
			}
			return "data:" + mediaType + ";base64," + data
		}
	}
	return strings.TrimSpace(block.URL)
}

// normalizeToolDefinitions renders the OpenAI function envelope for the
// request's tool declarations. The `model` argument is unused and kept only so
// the call sites read uniformly with the other request builders: normalization
// is model independent and lives in internal/util, shared with the WorkBuddy
// and Cline channels.
//
// The NoTools / no-declaration guard stays here rather than in the shared
// helper: Qoder distinguishes "no tools" (nil, so the gateway sees an empty
// array via the chatBody fallback below) from "tools present", while WorkBuddy
// always keeps a non-nil slice.
func normalizeToolDefinitions(req upstream.UpstreamRequest, model modelEntry) []interface{} {
	if req.NoTools || len(req.Tools) == 0 {
		return nil
	}
	return util.NormalizeToolDefinitions(req.Tools)
}

// normalizeToolControls maps both OpenAI and Anthropic tool selection shapes to
// the OpenAI-compatible fields accepted by Qoder's chat endpoint. Tool controls
// belong at the top level of the request; placing tool_choice under parameters
// makes the gateway treat an otherwise valid tool request as ordinary chat.
func normalizeToolControls(req upstream.UpstreamRequest, toolsEnabled bool) (interface{}, *bool) {
	if !toolsEnabled || req.NoTools {
		return nil, nil
	}

	parallel := cloneBool(req.ParallelToolCalls)
	choice := req.ToolChoice
	if choice == nil {
		return "auto", parallel
	}

	switch typed := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "auto", "required", "none":
			return strings.ToLower(strings.TrimSpace(typed)), parallel
		default:
			return "auto", parallel
		}
	case map[string]interface{}:
		kind := strings.ToLower(strings.TrimSpace(util.StringValue(typed["type"])))
		if disable, ok := typed["disable_parallel_tool_use"].(bool); ok && parallel == nil {
			value := !disable
			parallel = &value
		}
		switch kind {
		case "auto":
			return "auto", parallel
		case "any", "required":
			return "required", parallel
		case "none":
			return "none", parallel
		case "tool":
			name := strings.TrimSpace(util.StringValue(typed["name"]))
			if name == "" {
				return "auto", parallel
			}
			return map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": name},
			}, parallel
		case "function":
			return typed, parallel
		default:
			return "auto", parallel
		}
	default:
		return "auto", parallel
	}
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
