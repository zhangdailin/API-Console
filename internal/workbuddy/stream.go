package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// streamResult accumulates what the upstream produced so the caller can decide
// whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	// Model is the model identifier the upstream reported on its chunks.
	Model              string
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

type streamChoice struct {
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
}

type streamChunk struct {
	upstream.ChatStreamChunk[streamChoice]
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, onMessage func(upstream.SSEMessage), streamToolDeltas ...bool) (streamResult, error) {
	result := streamResult{}
	tools := util.NewToolCallAccumulator()
	sawDone := false
	sawFinish := false

	emitText := func(text string) { upstream.EmitTextDelta(onMessage, text, &result.SawMeaningfulEvent) }

	emitTools := func() error {
		return upstream.FinishStreamedTools(onMessage, tools, &result.SawMeaningfulEvent, &result.ToolCallCount)
	}

	var scanErr error
	for payload, data := range upstream.ScanChatStream(body, &scanErr) {
		if !data {
			// A 200 plain JSON business envelope should keep its upstream code.
			var env envelope
			if json.Unmarshal(payload, &env) == nil && env.Code != 0 {
				return result, apiError(http.StatusOK, payload)
			}
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			sawDone = true
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return result, fmt.Errorf("workbuddy stream protocol error: invalid JSON: %w", err)
		}
		if model := strings.TrimSpace(chunk.Model); model != "" {
			result.Model = model
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

		upstream.EmitReasoningDelta(onMessage, delta.ReasoningContent, "workbuddy-v1", &result.SawMeaningfulEvent, &result.ThinkingSignature)
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
	if scanErr != nil {
		return result, fmt.Errorf("failed to read workbuddy stream: %w", scanErr)
	}
	if !sawDone || !sawFinish {
		return result, fmt.Errorf("workbuddy stream ended before terminal finish")
	}
	return result, nil
}
