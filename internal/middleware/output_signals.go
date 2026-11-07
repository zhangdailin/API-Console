package middleware

import (
	"encoding/json"
	"strings"
)

// outputSignals keeps evidence only, never model text or tool arguments.
type outputSignals struct {
	visible, reasoning, tool bool
	finish                   string
	// units sizes the generated text of the event in quarter tokens; see
	// outputUnits.
	units int64
	// input is the prompt size a stream announces before generating
	// (Messages message_start usage.input_tokens).
	input int64
}

func streamOutputSignals(event string, data []byte) outputSignals {
	var p map[string]json.RawMessage
	if json.Unmarshal(data, &p) != nil {
		return outputSignals{}
	}
	var kind string
	_ = json.Unmarshal(p["type"], &kind)
	if kind == "" {
		kind = event
	}
	s := outputSignals{}
	switch kind {
	case "response.output_text.delta", "response.refusal.delta":
		s.visible = s.text(p["delta"])
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		s.reasoning = s.text(p["delta"])
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		s.tool = s.text(p["delta"])
	case "content_block_delta", "content_block_start":
		var d map[string]json.RawMessage
		key := "delta"
		if kind == "content_block_start" {
			key = "content_block"
		}
		_ = json.Unmarshal(p[key], &d)
		s.visible = s.text(d["text"])
		s.reasoning = s.text(d["thinking"])
		s.tool = s.text(d["partial_json"]) || kind == "content_block_start" && nonemptyJSONString(d["name"])
	case "message_start":
		var m struct {
			Usage struct {
				InputTokens int64 `json:"input_tokens"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(p["message"], &m)
		s.input = max(m.Usage.InputTokens, 0)
	case "message_delta":
		var d struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(p["delta"], &d)
		s.finish = d.StopReason
	case "response.completed", "response.incomplete":
		var d struct {
			Status            string
			IncompleteDetails struct{ Reason string } `json:"incomplete_details"`
		}
		_ = json.Unmarshal(p["response"], &d)
		if d.Status == "incomplete" {
			s.finish = d.IncompleteDetails.Reason
		} else if d.Status == "completed" {
			s.finish = "stop"
		}
	}
	var choices []struct {
		Finish string `json:"finish_reason"`
		Delta  struct {
			Content, Reasoning, Refusal string
			ReasoningContent            string            `json:"reasoning_content"`
			ToolCalls                   []json.RawMessage `json:"tool_calls"`
			FunctionCall                json.RawMessage   `json:"function_call"`
		}
	}
	_ = json.Unmarshal(p["choices"], &choices)
	for _, c := range choices {
		s.visible = s.visible || c.Delta.Content != "" || c.Delta.Refusal != ""
		s.reasoning = s.reasoning || c.Delta.Reasoning != "" || c.Delta.ReasoningContent != ""
		// Some upstreams send reasoning under both names; count it once.
		reasoning := c.Delta.ReasoningContent
		if reasoning == "" {
			reasoning = c.Delta.Reasoning
		}
		s.units += outputUnits(c.Delta.Content) + outputUnits(c.Delta.Refusal) + outputUnits(reasoning)
		for _, raw := range c.Delta.ToolCalls {
			var call struct {
				Function struct{ Name, Arguments string }
			}
			_ = json.Unmarshal(raw, &call)
			s.tool = s.tool || call.Function.Name != "" || call.Function.Arguments != ""
			s.units += outputUnits(call.Function.Arguments)
		}
		var function struct{ Name, Arguments string }
		_ = json.Unmarshal(c.Delta.FunctionCall, &function)
		s.tool = s.tool || function.Name != "" || function.Arguments != ""
		if c.Finish != "" {
			s.finish = c.Finish
		}
	}
	return s
}

// text decodes a JSON string field, adds its size to the event's units and
// reports whether it was non-empty.
func (s *outputSignals) text(raw json.RawMessage) bool {
	var text string
	if len(raw) == 0 || json.Unmarshal(raw, &text) != nil || text == "" {
		return false
	}
	s.units += outputUnits(text)
	return true
}

// outputUnits estimates the tokens of generated text, in quarter tokens so the
// many tiny deltas of a stream do not each round up: an ASCII byte is a
// quarter token and any other character a whole one, roughly what BPE
// tokenizers produce for English and CJK text. It only paces the live output
// rate; the upstream's usage replaces it once the request reports it.
func outputUnits(text string) int64 {
	var units int64
	for _, r := range text {
		if r < 0x80 {
			units++
		} else {
			units += 4
		}
	}
	return units
}

func isTokenTruncation(reason string) bool {
	switch strings.ToLower(reason) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	}
	return false
}
