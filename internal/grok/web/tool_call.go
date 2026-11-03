package web

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	toolCallBlockRE         = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	toolCallFenceStartRE    = regexp.MustCompile("^```[a-zA-Z0-9_-]*\\s*")
	toolCallFenceEndRE      = regexp.MustCompile("\\s*```$")
	toolCallTrailingCommaRE = regexp.MustCompile(`,\s*([}\]])`)
)

type toolCallParser struct {
	forcedTool string
	validNames map[string]struct{}
}

func newToolCallParser(tools []ToolDef, toolChoice interface{}) toolCallParser {
	validNames := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			continue
		}
		if name := strings.TrimSpace(fmt.Sprint(tool.Function["name"])); name != "" {
			validNames[name] = struct{}{}
		}
	}
	return toolCallParser{
		forcedTool: forcedToolName(toolChoice),
		validNames: validNames,
	}
}

func forcedToolName(toolChoice interface{}) string {
	choice, ok := toolChoice.(map[string]interface{})
	if !ok {
		return ""
	}
	if strings.TrimSpace(fmt.Sprint(choice["type"])) != "function" {
		return ""
	}
	fn, _ := choice["function"].(map[string]interface{})
	return strings.TrimSpace(fmt.Sprint(fn["name"]))
}

func buildToolPrompt(tools []ToolDef, toolChoice interface{}, parallelToolCalls bool) string {
	if len(tools) == 0 {
		return ""
	}
	if choice, ok := toolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(choice), "none") {
		return ""
	}

	lines := []string{
		"# Available Tools",
		"",
		"You have access to the following tools. To call a tool, output a <tool_call> block with JSON containing \"name\" and \"arguments\".",
		"",
		"Format:",
		"<tool_call>",
		`{"name":"function_name","arguments":{"param":"value"}}`,
		"</tool_call>",
		"",
	}
	if parallelToolCalls {
		lines = append(lines,
			"You may make multiple tool calls in a single response by using multiple <tool_call> blocks.",
			"",
		)
	}
	lines = append(lines, "## Tool Definitions", "")
	for _, tool := range tools {
		if !strings.EqualFold(strings.TrimSpace(tool.Type), "function") {
			continue
		}
		name := strings.TrimSpace(fmt.Sprint(tool.Function["name"]))
		if name == "" {
			continue
		}
		lines = append(lines, "### "+name)
		if desc := strings.TrimSpace(fmt.Sprint(tool.Function["description"])); desc != "" {
			lines = append(lines, desc)
		}
		if params, ok := tool.Function["parameters"]; ok && params != nil {
			if raw, err := json.Marshal(params); err == nil {
				lines = append(lines, "Parameters: "+string(raw))
			}
		}
		lines = append(lines, "")
	}
	forced := false
	switch choice := toolChoice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(choice)) {
		case "required":
			lines = append(lines, "IMPORTANT: You MUST call at least one tool in your response. Do not respond with only text.")
			forced = true
		}
	case map[string]interface{}:
		if name := forcedToolName(choice); name != "" {
			lines = append(lines, fmt.Sprintf("IMPORTANT: You MUST call the tool %q in your response.", name))
			forced = true
		}
	}
	if !forced {
		lines = append(lines, "Decide whether to call a tool based on the user's request. If you don't need a tool, respond normally with text only.")
	}
	lines = append(lines, "", "When you call a tool, you may include text before or after the <tool_call> blocks, but the tool call blocks must be valid JSON.")
	return strings.Join(lines, "\n")
}

func stripToolCallCodeFences(text string) string {
	cleaned := strings.TrimSpace(text)
	if !strings.HasPrefix(cleaned, "```") {
		return cleaned
	}
	cleaned = toolCallFenceStartRE.ReplaceAllString(cleaned, "")
	cleaned = toolCallFenceEndRE.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func extractToolCallJSONObject(text string) string {
	start := strings.Index(text, "{")
	if start < 0 {
		return text
	}
	end := strings.LastIndex(text, "}")
	if end < start {
		return text[start:]
	}
	return text[start : end+1]
}

func balanceJSONBraces(text string) string {
	openCount := 0
	closeCount := 0
	inString := false
	escape := false
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if escape {
			escape = false
			continue
		}
		if ch == '\\' && inString {
			escape = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			openCount++
		case '}':
			closeCount++
		}
	}
	if openCount > closeCount {
		text += strings.Repeat("}", openCount-closeCount)
	}
	return text
}

func repairToolCallJSON(text string) map[string]interface{} {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	cleaned := stripToolCallCodeFences(text)
	cleaned = extractToolCallJSONObject(cleaned)
	cleaned = strings.ReplaceAll(cleaned, "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\n", " ")
	cleaned = toolCallTrailingCommaRE.ReplaceAllString(cleaned, "$1")
	cleaned = balanceJSONBraces(cleaned)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
		return nil
	}
	return parsed
}

func (p toolCallParser) parseBlock(rawJSON string) map[string]interface{} {
	if strings.TrimSpace(rawJSON) == "" {
		return nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rawJSON), &parsed); err != nil {
		parsed = repairToolCallJSON(rawJSON)
	}
	if parsed == nil {
		return nil
	}

	name := strings.TrimSpace(fmt.Sprint(parsed["name"]))
	if name == "" {
		return nil
	}
	if len(p.validNames) > 0 {
		if _, ok := p.validNames[name]; !ok {
			return nil
		}
	}
	if p.forcedTool != "" && name != p.forcedTool {
		return nil
	}

	args := parsed["arguments"]
	argsRaw := "{}"
	if args != nil {
		switch v := args.(type) {
		case string:
			argsRaw = v
		default:
			if buf, err := json.Marshal(v); err == nil {
				argsRaw = string(buf)
			}
		}
	}
	return map[string]interface{}{
		"id":   "call_" + randomHex(12),
		"type": "function",
		"function": map[string]interface{}{
			"name":      name,
			"arguments": argsRaw,
		},
	}
}

func formatToolHistory(messages []ChatMessage) []ChatMessage {
	if len(messages) == 0 {
		return messages
	}
	out := make([]ChatMessage, 0, len(messages))
	for _, msg := range messages {
		role := normalizeMessageRole(msg.Role)
		switch {
		case role == "assistant" && len(msg.ToolCalls) > 0:
			parts := make([]string, 0, len(msg.ToolCalls)+1)
			if text := strings.TrimSpace(extractContentText(msg.Content)); text != "" {
				parts = append(parts, text)
			}
			for _, tc := range msg.ToolCalls {
				name := strings.TrimSpace(fmt.Sprint(tc.Function["name"]))
				if name == "" {
					continue
				}
				args := "{}"
				if raw := tc.Function["arguments"]; raw != nil {
					switch v := raw.(type) {
					case string:
						if strings.TrimSpace(v) != "" {
							args = strings.TrimSpace(v)
						}
					default:
						if buf, err := json.Marshal(v); err == nil {
							args = string(buf)
						}
					}
				}
				parts = append(parts, fmt.Sprintf(`<tool_call>{"name":"%s","arguments":%s}</tool_call>`, name, args))
			}
			out = append(out, ChatMessage{
				Role:    "assistant",
				Content: toolHistoryContentWithImages(strings.Join(parts, "\n"), msg.Content),
			})
		case role == "tool":
			content := strings.TrimSpace(extractContentText(msg.Content))
			name := strings.TrimSpace(msg.Name)
			if name == "" {
				name = "unknown"
			}
			callID := strings.TrimSpace(msg.ToolCallID)
			out = append(out, ChatMessage{
				Role:    "user",
				Content: toolHistoryContentWithImages(strings.TrimSpace(fmt.Sprintf("tool (%s, %s): %s", name, callID, content)), msg.Content),
			})
		default:
			out = append(out, msg)
		}
	}
	return out
}

// Web emulates tool history in text, but images must remain attachments rather
// than becoming a printed Go map (or disappearing from assistant history).
func toolHistoryContentWithImages(text string, original interface{}) interface{} {
	parts := []interface{}{map[string]interface{}{"type": "text", "text": text}}
	for _, part := range interfaceMaps(original) {
		if parseLooseStringAny(part["type"]) == "image_url" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 1 {
		return text
	}
	return parts
}

func (p toolCallParser) parseCalls(content string) (string, []map[string]interface{}) {
	if strings.TrimSpace(content) == "" {
		return content, nil
	}
	matches := toolCallBlockRE.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}

	toolCalls := make([]map[string]interface{}, 0, len(matches))
	textParts := make([]string, 0, len(matches)+1)
	lastEnd := 0
	for _, m := range matches {
		if len(m) < 4 {
			continue
		}
		before := strings.TrimSpace(content[lastEnd:m[0]])
		if before != "" {
			textParts = append(textParts, before)
		}
		block := strings.TrimSpace(content[m[2]:m[3]])
		lastEnd = m[1]
		if toolCall := p.parseBlock(block); toolCall != nil {
			toolCalls = append(toolCalls, toolCall)
		}
	}
	if trailing := strings.TrimSpace(content[lastEnd:]); trailing != "" {
		textParts = append(textParts, trailing)
	}
	if len(toolCalls) == 0 {
		return content, nil
	}
	return strings.Join(textParts, "\n"), toolCalls
}

// toolStreamPump incrementally splits a text stream into plain-text deltas and
// complete <tool_call> blocks. streamChat feeds token/message chunks through it
// so the tag state machine stays out of the main event loop.
type toolStreamPump struct {
	parser           toolCallParser
	state            string // "text" | "tool"
	buffer           string
	partial          string
	SawEvents        bool
	EmittedToolCalls bool
}

func newToolStreamPump(parser toolCallParser) *toolStreamPump {
	return &toolStreamPump{parser: parser, state: "text"}
}

func (p *toolStreamPump) feed(chunk string, onText func(string), onTool func(map[string]interface{})) {
	if p == nil || chunk == "" {
		return
	}
	const startTag = "<tool_call>"
	const endTag = "</tool_call>"
	data := p.partial + chunk
	p.partial = ""

	for data != "" {
		if p.state == "text" {
			startIdx := strings.Index(data, startTag)
			if startIdx < 0 {
				keep := suffixPrefixOverlap(data, startTag)
				emit := data
				if keep > 0 {
					emit = data[:len(data)-keep]
					p.partial = data[len(data)-keep:]
				}
				if strings.TrimSpace(emit) != "" {
					onText(emit)
					p.SawEvents = true
				}
				break
			}
			if before := data[:startIdx]; strings.TrimSpace(before) != "" {
				onText(before)
				p.SawEvents = true
			}
			data = data[startIdx+len(startTag):]
			p.state = "tool"
			continue
		}

		endIdx := strings.Index(data, endTag)
		if endIdx < 0 {
			keep := suffixPrefixOverlap(data, endTag)
			appendPart := data
			if keep > 0 {
				appendPart = data[:len(data)-keep]
				p.partial = data[len(data)-keep:]
			}
			p.buffer += appendPart
			break
		}

		p.buffer += data[:endIdx]
		if toolCall := p.parser.parseBlock(p.buffer); toolCall != nil {
			onTool(toolCall)
			p.EmittedToolCalls = true
			p.SawEvents = true
		}
		p.buffer = ""
		data = data[endIdx+len(endTag):]
		p.state = "text"
	}
}

func (p *toolStreamPump) flush(onText func(string), onTool func(map[string]interface{})) {
	if p == nil {
		return
	}
	if p.state == "text" {
		if strings.TrimSpace(p.partial) != "" {
			onText(p.partial)
			p.SawEvents = true
		}
		p.partial = ""
		return
	}
	raw := p.buffer + p.partial
	if toolCall := p.parser.parseBlock(raw); toolCall != nil {
		onTool(toolCall)
		p.EmittedToolCalls = true
		p.SawEvents = true
	} else if strings.TrimSpace(raw) != "" {
		onText("<tool_call>" + raw)
		p.SawEvents = true
	}
	p.buffer = ""
	p.partial = ""
	p.state = "text"
}
