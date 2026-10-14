package prompt

import (
	"fmt"
	"strings"

	"encoding/json"
)

// Shared message schemas and content decoding for caching and handlers.

// ImageSource describes where an image comes from.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url,omitempty"`
}

// CacheControl is the cache-control marker.
type CacheControl struct {
	Type string `json:"type"`
}

// ContentBlock is one block of a message body.
type ContentBlock struct {
	ToolIndex *int         `json:"index,omitempty"`
	Type      string       `json:"type"`
	Text      string       `json:"text,omitempty"`
	Source    *ImageSource `json:"source,omitempty"`
	URL       string       `json:"url,omitempty"`

	// tool_use fields
	ID        string      `json:"id,omitempty"`
	Name      string      `json:"name,omitempty"`
	Input     interface{} `json:"input,omitempty"`
	Thinking  string      `json:"thinking,omitempty"`
	Signature string      `json:"signature,omitempty"`

	// tool_result fields
	ToolUseID    string        `json:"tool_use_id,omitempty"`
	Content      interface{}   `json:"content,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// MessageContent is a union: either a string or a []ContentBlock.
type MessageContent struct {
	Text   string
	Blocks []ContentBlock
}

func (mc *MessageContent) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		mc.Text = ""
		mc.Blocks = nil
		return nil
	}
	trimmed := trimLeadingJSONSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("content must be string or array of content blocks")
	}
	if string(trimmed) == "null" {
		mc.Text = ""
		mc.Blocks = nil
		return nil
	}

	// Dispatch on the first byte rather than trial-unmarshalling. The array form is
	// the common one for a coding harness, and answering a `string` target with an
	// array makes encoding/json skip the whole array before reporting the type
	// error, so every block-carrying message body was scanned twice — and the
	// conversation is the bulk of a request that re-sends it on every turn.
	// SystemItems already dispatches this way; this is the same shape applied to
	// the larger field.
	switch trimmed[0] {
	case '"':
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return err
		}
		mc.Text = text
		mc.Blocks = nil
		return nil
	case '[':
		var blocks []ContentBlock
		if err := json.Unmarshal(trimmed, &blocks); err == nil {
			mc.Text = ""
			mc.Blocks = blocks
			return nil
		}
	}

	return fmt.Errorf("content must be string or array of content blocks")
}

// trimLeadingJSONSpace returns data without its leading JSON whitespace. It is a
// subslice, not a copy, so the dispatch above costs nothing.
func trimLeadingJSONSpace(data []byte) []byte {
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return data[i:]
	}
	return nil
}

func (mc MessageContent) MarshalJSON() ([]byte, error) {
	if mc.Blocks != nil {
		return json.Marshal(mc.Blocks)
	}
	return json.Marshal(mc.Text)
}

func (mc *MessageContent) IsString() bool            { return mc.Blocks == nil }
func (mc *MessageContent) GetText() string           { return mc.Text }
func (mc *MessageContent) GetBlocks() []ContentBlock { return mc.Blocks }

// ExtractText returns the concatenated text content of the message.
func (mc *MessageContent) ExtractText() string {
	if mc.IsString() {
		return strings.TrimSpace(mc.GetText())
	}
	var parts []string
	for _, block := range mc.GetBlocks() {
		if block.Type == "text" {
			text := strings.TrimSpace(block.Text)
			if text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// ExtractText is a helper to extract text directly from the prompt.Message.
func (m *Message) ExtractText() string { return m.Content.ExtractText() }

// Message is a single conversation message.
type Message struct {
	Role             string          `json:"role"`
	Content          MessageContent  `json:"content"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ReasoningItem    json.RawMessage `json:"reasoning_item,omitempty"`
}

type openAIToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	Index    *int                   `json:"index,omitempty"`
	ID       string                 `json:"id"`
	Type     string                 `json:"type"`
	Function openAIToolCallFunction `json:"function"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role             string           `json:"role"`
		Content          json.RawMessage  `json:"content"`
		ToolCalls        []openAIToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string           `json:"tool_call_id,omitempty"`
		ReasoningContent string           `json:"reasoning_content,omitempty"`
		ReasoningItem    json.RawMessage  `json:"reasoning_item,omitempty"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	m.Role = raw.Role
	m.ReasoningContent = raw.ReasoningContent
	m.ReasoningItem = append(json.RawMessage(nil), raw.ReasoningItem...)
	content := raw.Content
	if len(content) == 0 {
		content = json.RawMessage("null")
	}
	if err := json.Unmarshal(content, &m.Content); err != nil {
		return err
	}

	switch strings.ToLower(strings.TrimSpace(raw.Role)) {
	case "assistant":
		if len(raw.ToolCalls) == 0 {
			return nil
		}
		blocks := make([]ContentBlock, 0, len(raw.ToolCalls)+1)
		if m.Content.IsString() {
			if text := m.Content.GetText(); strings.TrimSpace(text) != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
		} else if len(m.Content.GetBlocks()) > 0 {
			blocks = append(blocks, m.Content.GetBlocks()...)
		}
		for _, call := range raw.ToolCalls {
			name := strings.TrimSpace(call.Function.Name)
			if name == "" {
				continue
			}
			blocks = append(blocks, ContentBlock{
				ToolIndex: call.Index,
				Type:      "tool_use",
				ID:        strings.TrimSpace(call.ID),
				Name:      name,
				Input:     decodeOpenAIToolArguments(call.Function.Arguments),
			})
		}
		m.Content = MessageContent{Blocks: blocks}
	case "tool":
		m.Role = "user"
		var content string
		if m.Content.IsString() {
			content = m.Content.GetText()
		} else {
			content = m.Content.ExtractText()
		}
		m.Content = MessageContent{Blocks: []ContentBlock{{
			Type:      "tool_result",
			ToolUseID: strings.TrimSpace(raw.ToolCallID),
			Content:   content,
		}}}
	}

	return nil
}

func decodeOpenAIToolArguments(raw string) interface{} {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]interface{}{}
	}

	var value interface{}
	if err := json.Unmarshal([]byte(trimmed), &value); err == nil {
		switch typed := value.(type) {
		case nil:
			return map[string]interface{}{}
		case map[string]interface{}:
			return typed
		default:
			return map[string]interface{}{"value": typed}
		}
	}

	return map[string]interface{}{"raw": trimmed}
}

// SystemItem is one entry of the system prompt.
type SystemItem struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}
