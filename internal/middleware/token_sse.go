package middleware

import (
	"bytes"
	"encoding/json"
	"orchids-api/internal/util"
	"strings"
)

func (w *TracedResponseWriter) isSSE() bool {
	return strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream")
}

// tokenSSEDetector parses complete events across arbitrary Write boundaries.
// Only generated text, reasoning or tool content contributes a TTFT sample.
// Oversized events are skipped with bounded memory, without changing the stream.
type tokenSSEDetector struct {
	visible, reasoning, tool bool
	finish                   string
	line                     []byte
	data                     []byte
	event                    string
	size                     int
	overflow                 bool
	lineNonempty             bool
}

const maxTokenEventBytes = 1 << 20

func (d *tokenSSEDetector) observe(chunk []byte) bool {
	anyGenerated := false
	for _, b := range chunk {
		if b != '\n' {
			if b != '\r' {
				d.lineNonempty = true
			}
			d.size++
			if d.size > maxTokenEventBytes {
				d.overflow = true
			}
			if !d.overflow {
				d.line = append(d.line, b)
			}
			continue
		}
		line := bytes.TrimSuffix(d.line, []byte{'\r'})
		if !d.lineNonempty {
			generated := !d.overflow && generatedTokenEvent(d.event, d.data)
			if !d.overflow {
				s := streamOutputSignals(d.event, d.data)
				d.visible = d.visible || s.visible
				d.reasoning = d.reasoning || s.reasoning
				d.tool = d.tool || s.tool
				if s.finish != "" {
					d.finish = s.finish
				}
			}
			*d = tokenSSEDetector{visible: d.visible, reasoning: d.reasoning, tool: d.tool, finish: d.finish}
			anyGenerated = anyGenerated || generated
			continue
		}
		if !d.overflow {
			field, value, _ := bytes.Cut(line, []byte{':'})
			value = bytes.TrimPrefix(value, []byte{' '})
			switch string(field) {
			case "event":
				d.event = string(value)
			case "data":
				if len(d.data) > 0 {
					d.data = append(d.data, '\n')
				}
				d.data = append(d.data, value...)
			}
		}
		d.line = nil
		d.lineNonempty = false
	}
	return anyGenerated
}

func generatedTokenEvent(event string, data []byte) bool {
	var p map[string]json.RawMessage
	if json.Unmarshal(data, &p) != nil {
		return false
	}
	var kind string
	_ = json.Unmarshal(p["type"], &kind)
	kind = util.FirstNonEmptyUntrimmed(kind, event)
	switch kind {
	case "response.output_text.delta", "response.reasoning_text.delta",
		"response.reasoning_summary_text.delta", "response.refusal.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return nonemptyJSONString(p["delta"])
	case "content_block_delta":
		var delta map[string]json.RawMessage
		_ = json.Unmarshal(p["delta"], &delta)
		return nonemptyJSONString(delta["text"]) || nonemptyJSONString(delta["thinking"]) || nonemptyJSONString(delta["partial_json"])
	case "content_block_start":
		var block map[string]json.RawMessage
		_ = json.Unmarshal(p["content_block"], &block)
		return nonemptyJSONString(block["text"]) || nonemptyJSONString(block["thinking"]) || nonemptyJSONString(block["name"])
	}
	// OpenAI Chat Completions has no event type. Role-only and usage chunks
	// contain no generated output. Tool names/arguments count; IDs alone do not.
	var choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			Refusal          string `json:"refusal"`
			ToolCalls        []struct {
				Function struct {
					Name      string
					Arguments string
				} `json:"function"`
			} `json:"tool_calls"`
			FunctionCall struct {
				Name      string
				Arguments string
			} `json:"function_call"`
		} `json:"delta"`
	}
	if json.Unmarshal(p["choices"], &choices) != nil {
		return false
	}
	for _, c := range choices {
		d := c.Delta
		if d.Content != "" || d.Reasoning != "" || d.ReasoningContent != "" || d.Refusal != "" || d.FunctionCall.Name != "" || d.FunctionCall.Arguments != "" {
			return true
		}
		for _, tool := range d.ToolCalls {
			if tool.Function.Name != "" || tool.Function.Arguments != "" {
				return true
			}
		}
	}
	return false
}

func nonemptyJSONString(raw json.RawMessage) bool {
	var s string
	return json.Unmarshal(raw, &s) == nil && s != ""
}
