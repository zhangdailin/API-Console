package upstream

import (
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/util"
)

// ChatMessage is a text-only OpenAI-shaped upstream message.
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the OpenAI function-call wire shape.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries a tool name and JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ConvertAssistantMessage maps text, thinking and tool-use blocks onto one
// assistant message and records calls awaiting tool results.
func ConvertAssistantMessage(msg prompt.Message, pendingToolCalls map[string]bool) (ChatMessage, bool) {
	message := ChatMessage{Role: "assistant", ReasoningContent: strings.TrimSpace(msg.ReasoningContent)}
	var text []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		case "thinking":
			if message.ReasoningContent == "" {
				message.ReasoningContent = strings.TrimSpace(block.Thinking)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" {
				id = util.NewToolCallID()
			}
			message.ToolCalls = append(message.ToolCalls, ToolCall{
				ID: id, Type: "function",
				Function: ToolCallFunction{Name: name, Arguments: util.CompactToolInput(block.Input)},
			})
			pendingToolCalls[id] = true
		}
	}
	message.Content = strings.Join(text, "\n")
	return message, message.Content != "" || len(message.ToolCalls) > 0 || message.ReasoningContent != ""
}
