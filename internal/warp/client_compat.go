package warp

import (
	"encoding/json"
	"strings"

	"orchids-api/internal/prompt"
)

// Client options are local compatibility hints, never Warp generation settings.
// Chat already returns terminal usage; it does not create stored responses.
func rawClientOptions(request map[string]json.RawMessage) error {
	if raw, exists := request["store"]; exists {
		var store bool
		if !rawBool(raw) || json.Unmarshal(raw, &store) != nil || store {
			return wireFieldError("store", "only false is supported; stored Chat responses are unavailable")
		}
	}
	if raw, exists := request["stream_options"]; exists {
		var options map[string]json.RawMessage
		if json.Unmarshal(raw, &options) != nil || options == nil || !rawKeys(options, "include_usage") {
			return wireFieldError("stream_options", "must be an object containing only include_usage")
		}
		if value, exists := options["include_usage"]; exists && !rawBool(value) {
			return wireFieldError("stream_options.include_usage", "must be a boolean")
		}
	}
	return nil
}

// Adjacent user text messages are context fragments of one turn. Preserve their
// order without promoting them to instructions or inventing assistant replies.
// Tool results stay typed when merged; their ownership checks still apply.
func mergeUserContext(messages []prompt.Message) ([]prompt.Message, bool) {
	merged := make([]prompt.Message, 0, len(messages))
	for i := 0; i < len(messages); {
		end := i + 1
		if messages[i].Role == "user" {
			for end < len(messages) && messages[end].Role == "user" {
				end++
			}
		}
		if end == i+1 {
			merged = append(merged, messages[i])
			i = end
			continue
		}
		var blocks []prompt.ContentBlock
		allText := true
		for j := i; j < end; j++ {
			message := messages[j]
			if message.ReasoningContent != "" || len(message.ReasoningItem) != 0 || (message.Content.Blocks != nil && message.Content.Text != "") {
				return nil, false
			}
			parts := message.Content.Blocks
			if parts == nil {
				parts = []prompt.ContentBlock{{Type: "text", Text: message.Content.Text}}
			}
			if len(parts) == 0 {
				return nil, false
			}
			for _, part := range parts {
				if !validMessageBlock(part, "user") {
					return nil, false
				}
				allText = allText && part.Type == "text"
			}
			if len(blocks) > 0 && blocks[len(blocks)-1].Type == "text" && parts[0].Type == "text" {
				blocks = append(blocks, prompt.ContentBlock{Type: "text", Text: "\n\n"})
			}
			blocks = append(blocks, parts...)
		}
		content := prompt.MessageContent{Blocks: blocks}
		if allText {
			var text strings.Builder
			for _, block := range blocks {
				text.WriteString(block.Text)
			}
			content = prompt.MessageContent{Text: text.String()}
		}
		merged = append(merged, prompt.Message{Role: "user", Content: content})
		i = end
	}
	return merged, true
}
