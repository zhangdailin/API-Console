package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

// These tests pin the observable behaviour of the Web chat stream and the
// non-streaming collector: what a client receives for a given grok.com NDJSON
// body. They exist so the two functions can be restructured safely.

func upstreamLines(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func line(response map[string]any) string {
	raw, _ := json.Marshal(map[string]any{"result": map[string]any{"response": response}})
	return string(raw)
}

type streamedChat struct {
	role, content, reasoning, finish string
	toolCalls                        []map[string]any
	errors                           []string
	done                             bool
	usage                            map[string]any
}

func readStream(t *testing.T, body string) streamedChat {
	t.Helper()
	var out streamedChat
	for frame := range strings.SplitSeq(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		data := ""
		for l := range strings.SplitSeq(frame, "\n") {
			if rest, ok := strings.CutPrefix(l, "data: "); ok {
				data += rest
			}
		}
		if data == "[DONE]" {
			out.done = true
			continue
		}
		var chunk struct {
			Error   *struct{ Message, Code string } `json:"error"`
			Usage   map[string]any                  `json:"usage"`
			Choices []struct {
				Delta struct {
					Role             string           `json:"role"`
					Content          string           `json:"content"`
					ReasoningContent string           `json:"reasoning_content"`
					ToolCalls        []map[string]any `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		testutil.NoError(t, json.Unmarshal([]byte(data), &chunk), "chunk is not JSON: "+data)
		if chunk.Error != nil {
			out.errors = append(out.errors, chunk.Error.Code+": "+chunk.Error.Message)
			continue
		}
		if len(chunk.Usage) > 0 {
			out.usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				out.role = choice.Delta.Role
			}
			out.content += choice.Delta.Content
			out.reasoning += choice.Delta.ReasoningContent
			out.toolCalls = append(out.toolCalls, choice.Delta.ToolCalls...)
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				out.finish = *choice.FinishReason
			}
		}
	}
	return out
}

func runStream(t *testing.T, tools []ToolDef, body *strings.Reader) streamedChat {
	t.Helper()
	recorder := httptest.NewRecorder()
	h := &Handler{}
	req := &ChatCompletionsRequest{Model: "grok-4", Stream: true, Tools: tools}
	h.streamChat(recorder, req, "grok-4", ModelSpec{ID: "grok-4"}, "token", "", false, tools, nil, body, nil)
	return readStream(t, recorder.Body.String())
}

func runCollect(t *testing.T, tools []ToolDef, body *strings.Reader) map[string]any {
	t.Helper()
	recorder := httptest.NewRecorder()
	h := &Handler{}
	req := &ChatCompletionsRequest{Model: "grok-4", Tools: tools}
	h.collectChat(recorder, req, "grok-4", ModelSpec{ID: "grok-4"}, "token", "", false, tools, nil, body, nil)
	var out map[string]any
	testutil.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out), "response is not JSON: "+recorder.Body.String())
	return out
}

func weatherTool() []ToolDef {
	return []ToolDef{{Type: "function", Function: map[string]any{"name": "get_weather", "parameters": map[string]any{"type": "object"}}}}
}

func TestWebStreamChatText(t *testing.T) {
	got := runStream(t, nil, upstreamLines(
		line(map[string]any{"token": "Hel", "responseId": "r1"}),
		line(map[string]any{"token": "lo", "responseId": "r1"}),
		line(map[string]any{"modelResponse": map[string]any{"message": "Hello world"}, "responseId": "r1"}),
	))
	testutil.Equal(t, got.role, "assistant")
	testutil.MustContain(t, got.content, "Hello world")
	testutil.Equal(t, got.finish, "stop")
	testutil.True(t, got.done, "the stream ends with [DONE]")
	testutil.Equal(t, len(got.errors), 0)
	testutil.True(t, got.usage != nil, "the final chunk carries usage")
}

func TestWebStreamChatTokenOnlyFallback(t *testing.T) {
	got := runStream(t, nil, upstreamLines(
		line(map[string]any{"token": "only "}),
		line(map[string]any{"token": "tokens"}),
	))
	testutil.MustContain(t, got.content, "only tokens")
	testutil.Equal(t, got.finish, "stop")
}

func TestWebStreamChatThinkingIsReasoning(t *testing.T) {
	got := runStream(t, nil, upstreamLines(
		line(map[string]any{"token": "let me think", "isThinking": true}),
		line(map[string]any{"token": "Answer"}),
		line(map[string]any{"modelResponse": map[string]any{"message": "Answer"}}),
	))
	testutil.Equal(t, got.reasoning, "let me think")
	testutil.MustNotContain(t, got.content, "let me think")
	testutil.MustContain(t, got.content, "Answer")
}

func TestWebStreamChatUpstreamErrorBeforeOutput(t *testing.T) {
	got := runStream(t, nil, upstreamLines(`{"error":{"message":"rate limited"}}`))
	testutil.Equal(t, len(got.errors), 1)
	// The upstream's text is classified into a public sentence, never echoed.
	testutil.MustContain(t, got.errors[0], "stream_error")
	testutil.MustContain(t, got.errors[0], "rate-limited")
}

func TestWebStreamChatUpstreamErrorAfterOutput(t *testing.T) {
	got := runStream(t, nil, upstreamLines(
		line(map[string]any{"token": "partial"}),
		`{"result":{"error":{"message":"boom"}}}`,
	))
	testutil.Equal(t, got.role, "assistant")
	testutil.Equal(t, len(got.errors), 1)
	testutil.MustNotContain(t, got.errors[0], "boom")
	testutil.Falsef(t, got.finish == "stop", "a failed stream must not finish as stop")
}

func TestWebStreamChatToolCall(t *testing.T) {
	call := `<tool_call>{"name":"get_weather","arguments":{"city":"Paris"}}</tool_call>`
	got := runStream(t, weatherTool(), upstreamLines(
		line(map[string]any{"token": call}),
		line(map[string]any{"modelResponse": map[string]any{"message": call}}),
	))
	testutil.Equal(t, got.finish, "tool_calls")
	testutil.Equal(t, len(got.toolCalls), 1)
	function, _ := got.toolCalls[0]["function"].(map[string]any)
	testutil.Equal(t, function["name"], any("get_weather"))
	testutil.MustContain(t, function["arguments"].(string), "Paris")
	testutil.MustNotContain(t, got.content, "<tool_call>")
	testutil.True(t, got.done, "the stream ends with [DONE]")
}

func TestWebStreamChatToolModeWithoutCall(t *testing.T) {
	got := runStream(t, weatherTool(), upstreamLines(
		line(map[string]any{"token": "No tool needed."}),
		line(map[string]any{"modelResponse": map[string]any{"message": "No tool needed."}}),
	))
	testutil.Equal(t, got.finish, "stop")
	testutil.Equal(t, len(got.toolCalls), 0)
	testutil.MustContain(t, got.content, "No tool needed.")
}

func TestWebCollectChatText(t *testing.T) {
	out := runCollect(t, nil, upstreamLines(
		line(map[string]any{"token": "Hel"}),
		line(map[string]any{"token": "lo"}),
		line(map[string]any{"modelResponse": map[string]any{"message": "Hello world"}, "responseId": "r1"}),
	))
	choices, _ := out["choices"].([]any)
	testutil.Equal(t, len(choices), 1)
	choice := choices[0].(map[string]any)
	message := choice["message"].(map[string]any)
	testutil.Equal(t, message["content"], any("Hello world"))
	testutil.Equal(t, choice["finish_reason"], any("stop"))
	testutil.True(t, out["usage"] != nil, "the response carries usage")
}

func TestWebCollectChatReasoning(t *testing.T) {
	out := runCollect(t, nil, upstreamLines(
		line(map[string]any{"token": "pondering", "isThinking": true}),
		line(map[string]any{"modelResponse": map[string]any{"message": "Done"}}),
	))
	message := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	testutil.Equal(t, message["content"], any("Done"))
	testutil.Equal(t, message["reasoning_content"], any("pondering"))
}

func TestWebCollectChatToolCall(t *testing.T) {
	call := `<tool_call>{"name":"get_weather","arguments":{"city":"Paris"}}</tool_call>`
	out := runCollect(t, weatherTool(), upstreamLines(
		line(map[string]any{"modelResponse": map[string]any{"message": "Checking. " + call}}),
	))
	choice := out["choices"].([]any)[0].(map[string]any)
	testutil.Equal(t, choice["finish_reason"], any("tool_calls"))
	calls := choice["message"].(map[string]any)["tool_calls"].([]any)
	testutil.Equal(t, len(calls), 1)
}

func TestWebCollectChatUpstreamError(t *testing.T) {
	recorder := httptest.NewRecorder()
	h := &Handler{}
	h.collectChat(recorder, &ChatCompletionsRequest{Model: "grok-4"}, "grok-4", ModelSpec{ID: "grok-4"}, "token", "", false, nil, nil,
		upstreamLines(`{"error":{"message":"denied"}}`), nil)
	testutil.True(t, recorder.Code >= 400, "an upstream error is not a success")
	testutil.MustNotContain(t, recorder.Body.String(), "denied")
	testutil.MustContain(t, recorder.Body.String(), `"error"`)
}
