package cline

import (
	"encoding/json"
	"strings"
	"testing"

	"orchids-api/internal/upstream"
)

const controlFallback = "<tool_call><function=write_stdin><parameter=chars>\x03</parameter><parameter=max_output_tokens>7000</parameter><parameter=session_id>11436</parameter><parameter=yield_time_ms>1000</parameter></function></tool_call>"

func controlFrame(delta map[string]interface{}, finish string) string {
	value := map[string]interface{}{"choices": []interface{}{map[string]interface{}{"index": 0, "delta": delta, "finish_reason": finish}}}
	raw, _ := json.Marshal(value)
	return "data: " + string(raw) + "\n\n"
}

func controlAnchor(args string) map[string]interface{} {
	return map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "id": "call-original", "function": map[string]interface{}{"name": "write_stdin", "arguments": args}}}}
}

func controlMessages(t *testing.T, stream string, enabled bool) (streamResult, []upstream.SSEMessage, error) {
	t.Helper()
	var messages []upstream.SSEMessage
	result, err := consumeStream(strings.NewReader(stream), enabled, func(msg upstream.SSEMessage) { messages = append(messages, msg) })
	return result, messages, err
}

func TestControlToolRecoveryAcrossFrames(t *testing.T) {
	// Production first emitted a native identity and only {"chars":, then
	// replayed the call in content. Test every content split, including tags.
	for split := 1; split < len(controlFallback); split++ {
		stream := controlFrame(controlAnchor(`{"chars": `), "") + controlFrame(map[string]interface{}{"content": controlFallback[:split]}, "") + controlFrame(map[string]interface{}{"content": controlFallback[split:]}, "tool_calls") + controlFrame(map[string]interface{}{}, "tool_calls") + "data: [DONE]\n\n"
		result, messages, err := controlMessages(t, stream, true)
		if err != nil || result.ToolCallCount != 1 || result.FinishReason() != "tool_use" {
			t.Fatalf("split %d: result=%+v err=%v", split, result, err)
		}
		if len(messages) != 1 || messages[0].Type != "model.tool-call" || messages[0].Event["toolCallId"] != "call-original" {
			t.Fatalf("split %d: unexpected delivery %+v", split, messages)
		}
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(messages[0].Event["input"].(string)), &args); err != nil {
			t.Fatal(err)
		}
		if args["chars"] != "\x03" || args["session_id"] != float64(11436) || args["max_output_tokens"] != float64(7000) || args["yield_time_ms"] != float64(1000) || len(args) != 4 {
			t.Fatalf("split %d: arguments changed %+v", split, args)
		}
	}
}

func TestControlToolRecoveryDoesNotInventCalls(t *testing.T) {
	for _, tc := range []struct {
		name, native string
		enabled      bool
	}{
		{"plain-text", "", true},
		{"disabled", controlFrame(controlAnchor(`{"chars": `), ""), false},
		{"valid-native", controlFrame(controlAnchor(`{"chars":"","session_id":11436}`), ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, messages, err := controlMessages(t, tc.native+controlFrame(map[string]interface{}{"content": controlFallback}, "stop")+"data: [DONE]\n\n", tc.enabled)
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for _, msg := range messages {
				if msg.Type == "model.text-delta" {
					text.WriteString(msg.Event["delta"].(string))
				}
				if msg.Type == "model.tool-call" && strings.Contains(msg.Event["input"].(string), `\u0003`) {
					t.Fatal("text created a control-character call")
				}
			}
			if text.String() != controlFallback || (tc.native == "" && result.ToolCallCount != 0) {
				t.Fatalf("ordinary text changed: %q, %+v", text.String(), result)
			}
		})
	}
}

func TestControlToolRecoveryTextBeforeNativeIdentity(t *testing.T) {
	split := strings.Index(controlFallback, "</parameter>") + len("</parameter")
	stream := controlFrame(map[string]interface{}{"content": controlFallback[:split]}, "") + controlFrame(controlAnchor(""), "") + controlFrame(map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"index": 0, "function": map[string]interface{}{"arguments": `{"chars": `}}}}, "") + controlFrame(map[string]interface{}{"content": controlFallback[split:]}, "tool_calls") + "data: [DONE]\n\n"
	result, messages, err := controlMessages(t, stream, true)
	if err != nil || result.ToolCallCount != 1 || len(messages) != 1 || messages[0].Type != "model.tool-call" || messages[0].Event["toolCallId"] != "call-original" {
		t.Fatalf("production ordering not recovered: %+v %+v %v", result, messages, err)
	}
}

func TestControlToolRecoveryRequiresUniqueNativeAnchor(t *testing.T) {
	second := controlAnchor(`{"chars": `)
	call := second["tool_calls"].([]interface{})[0].(map[string]interface{})
	call["index"], call["id"] = 1, "call-second"
	stream := controlFrame(controlAnchor(`{"chars": `), "") + controlFrame(second, "") + controlFrame(map[string]interface{}{"content": controlFallback}, "tool_calls")
	result, messages, err := controlMessages(t, stream, true)
	if err == nil || result.ToolCallCount != 0 || len(messages) != 0 {
		t.Fatalf("ambiguous anchor accepted: %+v %+v %v", result, messages, err)
	}
}

func TestControlToolRecoveryRejectsAmbiguousFallback(t *testing.T) {
	for name, text := range map[string]string{
		"duplicate":         strings.Replace(controlFallback, "</function>", "<parameter=session_id>42</parameter></function>", 1),
		"extra-call":        controlFallback + controlFallback,
		"truncated":         strings.TrimSuffix(controlFallback, "</tool_call>"),
		"negative-session":  strings.Replace(controlFallback, ">11436<", ">-1<", 1),
		"unknown-parameter": strings.Replace(controlFallback, "yield_time_ms", "command", 1),
		"oversize":          strings.Replace(controlFallback, ">7000<", ">"+strings.Repeat("7", 17*1024)+"<", 1),
	} {
		t.Run(name, func(t *testing.T) {
			result, messages, err := controlMessages(t, controlFrame(controlAnchor(`{"chars": `), "")+controlFrame(map[string]interface{}{"content": text}, "tool_calls"), true)
			if err == nil || result.ToolCallCount != 0 || len(messages) != 0 {
				t.Fatalf("invalid fallback delivered: %+v, %+v, %v", result, messages, err)
			}
		})
	}
}
