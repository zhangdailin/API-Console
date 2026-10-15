package grok

import (
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
)

var errQualityBufferLimit = errors.New("unverified native response exceeded the quality buffer limit")

// Native Responses use the same classifier as Chat, but never fail open with a
// response already classified as degraded. Releasing frames is irreversible.
type nativeResponseQuality struct {
	hold      *buildQualityHold
	result    *chatOutcome
	reasoning map[string]int64
	encrypted map[string]int64
}

func newNativeResponseQuality(w http.ResponseWriter, policy qualityHoldPolicy) *nativeResponseQuality {
	hold := newBuildQualityHold(w, policy, nil)
	hold.writer.failOnOverflow = true
	return &nativeResponseQuality{hold: hold, reasoning: map[string]int64{}, encrypted: map[string]int64{}}
}

func (q *nativeResponseQuality) observe(event map[string]any, kind string, result *chatOutcome) {
	if q == nil {
		return
	}
	q.result, q.hold.outcome = result, result
	result.Quality.ExpectReasoning = true
	if usage := consoleUsageFromStreamEvent(event); len(usage) > 0 {
		result.Usage = usage
	}
	if details, ok := result.Usage["completion_tokens_details"].(map[string]any); ok {
		result.Quality.ReasoningTokens = int64(responses.InterfaceToInt(details["reasoning_tokens"]))
	}
	switch kind {
	case "response.output_text.delta", "response.refusal.delta":
		if text, _ := event["delta"].(string); text != "" {
			if result.Quality.VisibleChars == 0 {
				result.Quality.FirstVisibleMS = time.Since(q.hold.started).Milliseconds()
			}
			result.Quality.VisibleChars += int64(utf8.RuneCountInString(text))
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if text, _ := event["delta"].(string); text != "" {
			result.Quality.ReasoningChars += int64(utf8.RuneCountInString(text))
			result.Quality.SawReasoning = true
		}
	case "response.output_item.added", "response.output_item.done":
		item, _ := event["item"].(map[string]any)
		if item["type"] == "reasoning" {
			q.hold.markReasoningStarted()
			id := chatwire.ParseLooseStringAny(item["id"])
			q.encrypted[id] = max(q.encrypted[id], int64(len(chatwire.ParseLooseStringAny(item["encrypted_content"]))))
			q.reasoning[id] = max(q.reasoning[id], int64(utf8.RuneCountInString(reasoningItemText(item))))
			var encrypted, reasoning int64
			for _, n := range q.encrypted {
				encrypted += n
			}
			for _, n := range q.reasoning {
				reasoning += n
			}
			result.Quality.EncryptedChars = encrypted
			result.Quality.ReasoningChars = max(result.Quality.ReasoningChars, reasoning)
			result.Quality.SawReasoning = result.Quality.ReasoningChars > 0
		}
	}
	response, _ := event["response"].(map[string]any)
	if response != nil && (kind == "response.completed" || kind == "response.incomplete") {
		q.observeResponse(response, result)
	}
}

func reasoningItemText(item map[string]any) string {
	// The extractor already prefers raw reasoning over its summary.
	return consoleExtractReasoningText(map[string]any{"output": []any{item}})
}

func (q *nativeResponseQuality) observeResponse(response map[string]any, result *chatOutcome) {
	if q == nil {
		return
	}
	q.result, q.hold.outcome = result, result
	result.Quality.ExpectReasoning = true
	result.Quality.VisibleChars = max(result.Quality.VisibleChars, int64(utf8.RuneCountInString(consoleExtractMessageText(response))))
	result.Quality.ReasoningChars = max(result.Quality.ReasoningChars, int64(utf8.RuneCountInString(consoleExtractReasoningText(response))))
	result.Quality.SawReasoning = result.Quality.ReasoningChars > 0
	result.Quality.EncryptedChars = max(result.Quality.EncryptedChars, int64(len(consoleExtractEncryptedReasoning(response))))
	if details, ok := result.Usage["completion_tokens_details"].(map[string]any); ok {
		result.Quality.ReasoningTokens = int64(responses.InterfaceToInt(details["reasoning_tokens"]))
	}
	for _, item := range responses.InterfaceMaps(response["output"]) {
		if item["type"] == "function_call" {
			result.Quality.ToolCalls++
		}
	}
}

func (q *nativeResponseQuality) check(terminal bool) error {
	if q == nil || q.result == nil || q.hold.writer.Committed() {
		return nil
	}
	if q.hold.writer.Buffered() >= qualityHoldMaxBytes {
		return errQualityBufferLimit
	}
	if terminal {
		q.result.Quality.Terminal = q.result.Err == nil
		// Function arguments are client work, not visible analysis. A legitimate
		// tool-only turn can consume many output tokens without a text answer.
		if q.result.Quality.VisibleChars == 0 && q.result.Quality.ToolCalls > 0 {
			return q.hold.writer.Commit()
		}
	}
	verdict, _ := q.hold.step(terminal)
	if verdict == qualityWithhold {
		// Wait for terminal usage and tool evidence before discarding a turn.
		if terminal {
			q.result.Withheld, q.result.WithheldReason = true, q.hold.reason
		}
		return nil
	}
	if verdict == qualityDeliver || terminal {
		return q.hold.writer.Commit()
	}
	return nil
}
