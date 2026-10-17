package workbuddy

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"

	"encoding/json"

	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// streamResult accumulates what the upstream produced so the caller can decide
// whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	SawMeaningfulEvent bool
	ToolCallCount      int
	Usage              map[string]any
	ThinkingSignature  string
	FinishReasonValue  string
}

// FinishReason maps the accumulated stream onto an Anthropic-style stop reason.
func (r streamResult) FinishReason() string {
	if r.ToolCallCount > 0 || strings.EqualFold(r.FinishReasonValue, "tool_calls") {
		return "tool_use"
	}
	switch strings.ToLower(strings.TrimSpace(r.FinishReasonValue)) {
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// streamChunk is one `data:` line of the SSE response.
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Code    int    `json:"code"`
	Msg     string `json:"msg"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
	Error struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, onMessage func(upstream.SSEMessage), streamToolDeltas ...bool) (streamResult, error) {
	scanner := bufio.NewScanner(body)
	buffer := util.AcquireStreamBuffer()
	defer util.ReleaseStreamBuffer(buffer)
	scanner.Buffer(buffer[:], 8*1024*1024)
	result := streamResult{}
	tools := util.NewToolCallAccumulator()
	sawDone := false
	sawFinish := false

	emitText := func(text string) { upstream.EmitTextDelta(onMessage, text, &result.SawMeaningfulEvent) }

	emitTools := func() error {
		calls := tools.CompleteAll()
		if err := upstream.ValidateStreamedTools(calls); err != nil {
			return err
		}
		upstream.EmitToolCalls(onMessage, calls, &result.SawMeaningfulEvent, &result.ToolCallCount)
		return nil
	}

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || bytes.HasPrefix(line, []byte(":")) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			// Some edge responses keep HTTP 200 but return the business envelope as
			// plain JSON rather than SSE. Preserve that code instead of degrading it
			// to a generic "no events" protocol error.
			if bytes.HasPrefix(line, []byte("{")) {
				var env envelope
				if json.Unmarshal(line, &env) == nil && env.Code != 0 {
					return result, apiError(http.StatusOK, line)
				}
			}
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(payload, []byte("[DONE]")) {
			sawDone = true
			break
		}
		if len(payload) == 0 {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return result, fmt.Errorf("workbuddy stream protocol error: invalid JSON: %w", err)
		}
		if chunk.Code != 0 {
			return result, apiError(http.StatusOK, payload)
		}
		if msg := strings.TrimSpace(chunk.Error.Message); msg != "" {
			return result, fmt.Errorf("workbuddy stream error: %s", msg)
		}
		upstream.ApplyStreamUsage(onMessage, upstream.NormalizeUsageMap(chunk.Usage), &result.SawMeaningfulEvent, &result.Usage)
		if len(chunk.Choices) == 0 {
			continue
		}
		if sawFinish {
			return result, fmt.Errorf("workbuddy stream protocol error: choice data after finish")
		}
		delta := chunk.Choices[0].Delta

		if delta.ReasoningContent != "" {
			result.SawMeaningfulEvent = true
			if onMessage != nil {
				if result.ThinkingSignature == "" {
					result.ThinkingSignature = newThinkingSignature()
				}
				onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]any{
					"delta":     delta.ReasoningContent,
					"signature": result.ThinkingSignature,
				}})
			}
		}
		if delta.Content != "" {
			emitText(delta.Content)
		}
		for _, call := range delta.ToolCalls {
			result.SawMeaningfulEvent = true
			state := tools.Add(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
			if len(streamToolDeltas) > 0 && streamToolDeltas[0] {
				upstream.EmitToolDelta(onMessage, state)
			}
		}
		// OpenAI-style tool arguments can span several deltas. Emitting on the
		// first delta loses every later fragment and produces invalid JSON. A
		// non-empty finish reason closes the choice; [DONE]/EOF is handled below.
		if reason := strings.TrimSpace(chunk.Choices[0].FinishReason); reason != "" {
			result.FinishReasonValue = reason
			sawFinish = true
			if err := emitTools(); err != nil {
				return result, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("failed to read workbuddy stream: %w", err)
	}
	if !sawDone || !sawFinish {
		return result, fmt.Errorf("workbuddy stream ended before terminal finish")
	}
	return result, nil
}

func newThinkingSignature() string { return util.NewThinkingSignature("workbuddy-v1") }
