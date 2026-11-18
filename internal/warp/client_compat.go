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
// Tool-bearing messages are never merged, so their ownership checks stay intact.
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
		var text strings.Builder
		for j := i; j < end; j++ {
			part, ok := messageText(messages[j], -1)
			separator := ""
			if j > i {
				separator = "\n\n"
			}
			// Include separators and the eventual transcript label in the bound.
			if !ok || text.Len()+len(separator)+len(part)+len("User: ") > transcriptLimit {
				return nil, false
			}
			text.WriteString(separator)
			text.WriteString(part)
		}
		merged = append(merged, prompt.Message{Role: "user", Content: prompt.MessageContent{Text: text.String()}})
		i = end
	}
	return merged, true
}
