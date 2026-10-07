package middleware

import (
	"encoding/json"
	"strings"
)

// outputSignals keeps evidence only, never model text or tool arguments.
type outputSignals struct {
	visible, reasoning, tool bool
	finish                   string
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
		s.visible = nonemptyJSONString(p["delta"])
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		s.reasoning = nonemptyJSONString(p["delta"])
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		s.tool = nonemptyJSONString(p["delta"])
	case "content_block_delta", "content_block_start":
		var d map[string]json.RawMessage
		key := "delta"
		if kind == "content_block_start" {
			key = "content_block"
		}
		_ = json.Unmarshal(p[key], &d)
		s.visible = nonemptyJSONString(d["text"])
		s.reasoning = nonemptyJSONString(d["thinking"])
		s.tool = nonemptyJSONString(d["partial_json"]) || nonemptyJSONString(d["name"])
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
		for _, raw := range c.Delta.ToolCalls {
			var call struct {
				Function struct{ Name, Arguments string }
			}
			_ = json.Unmarshal(raw, &call)
			s.tool = s.tool || call.Function.Name != "" || call.Function.Arguments != ""
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

func isTokenTruncation(reason string) bool {
	switch strings.ToLower(reason) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	}
	return false
}
