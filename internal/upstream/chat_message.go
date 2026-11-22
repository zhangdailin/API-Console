package upstream

import (
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/util"
)

// ChatMessage is an OpenAI-shaped upstream message. User turns with images
// use typed content parts; other messages keep their string content.
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
	parts            []chatContentPart
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

// ReasoningForReplay returns the reasoning an assistant turn carried as plain
// text, so a channel can send it back with the history; other roles carry none.
func ReasoningForReplay(msg prompt.Message) string {
	if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
		return strings.TrimSpace(msg.ReasoningContent)
	}
	return ""
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

// ConvertBlockMessage maps user/system blocks, splitting tool_result blocks
// into standalone `tool` messages so the assistant/tool pairing stays intact.
// filterText, when set, rewrites each text block before it is kept.
func ConvertBlockMessage(role string, msg prompt.Message, pendingToolCalls map[string]bool, filterText func(string) string) []ChatMessage {
	out, _ := convertBlockMessage(role, msg, pendingToolCalls, filterText, nil)
	return out
}

// ConvertUserMessage also preserves images. Pass the same budget for every
// user message in a request, including image parts from replayed history.
func ConvertUserMessage(msg prompt.Message, pendingToolCalls map[string]bool, images *ChatImageBudget) ([]ChatMessage, error) {
	return convertBlockMessage("user", msg, pendingToolCalls, nil, images)
}

func convertBlockMessage(role string, msg prompt.Message, pendingToolCalls map[string]bool, filterText func(string) string, images *ChatImageBudget) ([]ChatMessage, error) {
	blocks := msg.Content.GetBlocks()
	out := make([]ChatMessage, 0, len(blocks))
	var textParts []string
	var imageParts []chatContentPart
	flush := func() {
		if len(imageParts) != 0 {
			out = append(out, ChatMessage{Role: role, parts: imageParts})
			imageParts = nil
		} else if len(textParts) != 0 {
			out = append(out, ChatMessage{Role: role, Content: strings.Join(textParts, "\n")})
		}
		textParts = textParts[:0]
	}
	for _, block := range blocks {
		switch block.Type {
		case "text", "input_text":
			if block.Type == "input_text" && images == nil {
				continue
			}
			text := block.Text
			if filterText != nil {
				text = filterText(text)
			}
			if strings.TrimSpace(text) != "" {
				if len(imageParts) != 0 {
					imageParts = append(imageParts, chatContentPart{Type: "text", Text: text})
				} else {
					textParts = append(textParts, text)
				}
			}
		case "image", "image_url", "input_image":
			if images == nil {
				continue
			}
			url, ok := images.imageURL(block)
			if !ok {
				return nil, errInvalidChatImage
			}
			if len(imageParts) == 0 {
				for _, text := range textParts {
					imageParts = append(imageParts, chatContentPart{Type: "text", Text: text})
				}
			}
			imageParts = append(imageParts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url}})
		case "tool_result":
			flush()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" || !pendingToolCalls[toolID] {
				// A tool_result with no matching call in this history would be
				// rejected as an orphan; dropping it is the only safe answer.
				continue
			}
			delete(pendingToolCalls, toolID)
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    prompt.StringifyToolResult(block.Content),
			})
		}
	}
	flush()
	return out, nil
}
