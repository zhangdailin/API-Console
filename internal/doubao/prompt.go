package doubao

import (
	"encoding/json"
	"fmt"
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

// The web client sends one text turn, so the whole Chat Completions request is
// flattened into it: the conversation, the declared functions and the output
// contract. Everything the OpenAI wire carries in a structured field has to
// appear in this text or the model never sees it.

// renderPrompt flattens the conversation, tool calls and tool results included.
func renderPrompt(req upstream.UpstreamRequest) string {
	var out strings.Builder
	for _, item := range req.System {
		if text := strings.TrimSpace(item.Text); text != "" {
			fmt.Fprintf(&out, "[system]\n%s\n\n", text)
		}
	}
	toolNames := map[string]string{}
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "developer" {
			role = "system"
		}
		for _, part := range renderMessage(role, message, toolNames) {
			fmt.Fprintf(&out, "[%s]\n%s\n\n", part.role, part.text)
		}
	}
	return strings.TrimSpace(out.String())
}

type promptSection struct{ role, text string }

// renderMessage turns one message into prompt sections. An assistant tool call
// is written in the envelope the model is told to answer with, and a tool
// result is labelled with the function it answers: without both, a turn that
// follows a call asks the same question again and the model calls again.
func renderMessage(role string, message prompt.Message, toolNames map[string]string) []promptSection {
	if message.Content.IsString() {
		if text := strings.TrimSpace(message.Content.GetText()); text != "" {
			return []promptSection{{role, text}}
		}
		return nil
	}
	var sections []promptSection
	var texts []string
	var calls []map[string]any
	flush := func() {
		if len(texts) > 0 {
			sections = append(sections, promptSection{role, strings.Join(texts, "\n")})
			texts = nil
		}
	}
	for _, block := range message.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if text := strings.TrimSpace(block.Text); text != "" {
				texts = append(texts, text)
			}
		case "tool_use":
			toolNames[block.ID] = block.Name
			arguments := block.Input
			if arguments == nil {
				arguments = map[string]any{}
			}
			calls = append(calls, map[string]any{"name": block.Name, "arguments": arguments})
		case "tool_result":
			flush()
			label := "tool result"
			if name := toolNames[block.ToolUseID]; name != "" {
				label = "tool result of " + name
			}
			sections = append(sections, promptSection{label, prompt.StringifyToolResult(block.Content)})
		}
	}
	if len(calls) > 0 {
		if raw, err := json.Marshal(map[string]any{"tool_calls": calls}); err == nil {
			texts = append(texts, string(raw))
		}
	}
	flush()
	return sections
}

// toolChoice reads the OpenAI and Anthropic spellings of tool_choice.
func toolChoice(choice any) (mode, name string) {
	switch typed := choice.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(typed)), ""
	case map[string]any:
		mode, _ = typed["type"].(string)
		mode = strings.ToLower(strings.TrimSpace(mode))
		name, _ = typed["name"].(string)
		if function, ok := typed["function"].(map[string]any); ok && name == "" {
			name, _ = function["name"].(string)
		}
		if mode == "any" {
			mode = "required"
		}
		return mode, strings.TrimSpace(name)
	}
	return "", ""
}

// usesTools reports whether the turn is answered in the tool envelope.
// tool_choice "none" asks for a plain answer even though functions are declared.
func usesTools(req upstream.UpstreamRequest) bool {
	mode, _ := toolChoice(req.ToolChoice)
	return len(req.Tools) > 0 && !req.NoTools && mode != "none"
}

func toolInstruction(tools []any, choice any) string {
	raw, _ := json.Marshal(tools)
	instruction := "\n\nYou must answer using exactly one JSON object. For a normal answer return {\"content\":\"your answer\"}. To call a function return {\"tool_calls\":[{\"name\":\"declared function name\",\"arguments\":{}}]}. Do not use markdown."
	switch mode, name := toolChoice(choice); {
	case name != "":
		instruction += fmt.Sprintf(" You must call the function %q in this answer.", name)
	case mode == "required":
		instruction += " You must call one of the declared functions in this answer."
	}
	return instruction + " Declared functions:\n" + string(raw)
}

// formatInstruction states a response_format the upstream has no field for.
// A strict schema is instructed and validated by the shared handler, so only
// json_object and a non-strict schema are handled here.
func formatInstruction(req upstream.UpstreamRequest) string {
	if _, strict := req.StrictSchema(); strict {
		return ""
	}
	format := req.ChatResponseFormat()
	kind, _ := format["type"].(string)
	const jsonOnly = "\n\nRespond with one valid JSON object only: no prose, no explanation and no markdown code fence."
	switch kind {
	case "json_object":
		return jsonOnly
	case "json_schema":
		spec, _ := format["json_schema"].(map[string]any)
		if raw, err := json.Marshal(spec["schema"]); err == nil && spec["schema"] != nil {
			return jsonOnly + " It should follow this JSON Schema:\n" + string(raw)
		}
		return jsonOnly
	}
	return ""
}

// jsonCandidate returns the JSON object inside an answer. The model is asked
// for bare JSON but often wraps it in a code fence or a sentence.
func jsonCandidate(text string) string {
	text = strings.TrimSpace(text)
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return text
	}
	return text[start : end+1]
}

// emitToolOrText turns a tool-mode answer into tool calls or text, and reports
// whether it emitted a call.
func emitToolOrText(text string, tools []any, emit func(upstream.SSEMessage)) bool {
	if emit == nil {
		return false
	}
	var envelope struct {
		Content   *string `json:"content"`
		ToolCalls []struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"tool_calls"`
	}
	parsed := json.Unmarshal([]byte(jsonCandidate(text)), &envelope) == nil
	if parsed && len(envelope.ToolCalls) > 0 {
		allowed := map[string]bool{}
		for _, tool := range tools {
			row, _ := tool.(map[string]any)
			function, _ := row["function"].(map[string]any)
			if name, _ := function["name"].(string); name != "" {
				allowed[name] = true
			}
		}
		emitted := false
		for _, call := range envelope.ToolCalls {
			var args map[string]any
			if !allowed[call.Name] || json.Unmarshal(call.Arguments, &args) != nil || args == nil {
				continue
			}
			emit(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]any{"toolCallId": "call_" + randomHex(8), "toolName": call.Name, "input": string(call.Arguments)}})
			emitted = true
		}
		if emitted {
			return true
		}
	}
	if parsed && envelope.Content != nil && len(envelope.ToolCalls) == 0 {
		text = *envelope.Content
	}
	emit(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": text}})
	return false
}
