package qoder

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// The upstream always answers with SSE, and the payload is doubly wrapped: each
// `data:` line is an envelope object whose `body` field is itself a JSON string
// holding an OpenAI-shaped chunk. Reading the envelope as the chunk silently
// produces an empty answer, so the two layers are unwrapped explicitly.
//
// Termination is also not the OpenAI one. The stream ends with `event:finish`;
// a `[DONE]` marker may appear before final usage. Only event:finish terminates
// the stream; EOF before it is a truncation, not a successful completion.

// streamEnvelope is the outer SSE frame.
type streamEnvelope struct {
	Headers         map[string][]string `json:"headers"`
	Body            string              `json:"body"`
	StatusCodeValue int                 `json:"statusCodeValue"`
	StatusCode      string              `json:"statusCode"`
}

// streamChunk is the inner OpenAI-shaped chunk.
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
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
	Usage *streamUsage `json:"usage"`
	Error *struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
}

type streamUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens    int `json:"cached_tokens"`
		CacheableTokens int `json:"cacheable_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// The gateway bills in credits and reports the discount separately; the
	// numbers are surfaced so a cost panel can show what a plan actually cost.
	Credits         *float64 `json:"credits"`
	OriginalCredits *float64 `json:"original_credits"`
}

// streamResult accumulates what one upstream stream produced, so the caller can
// decide whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	SawMeaningfulEvent bool
	ToolCallCount      int
	FinishReasonValue  string
	Usage              map[string]interface{}
	ThinkingSignature  string
}

// FinishReason maps the accumulated stream onto an Anthropic-style stop reason.
func (r streamResult) FinishReason() string {
	switch strings.TrimSpace(r.FinishReasonValue) {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	}
	if r.ToolCallCount > 0 {
		return "tool_use"
	}
	return "end_turn"
}

// emitFinish closes the stream with the stop reason and the usage in one frame.
// Usage travels with the finish rather than as its own frame because a caller
// that sees the usage without the finish cannot tell a complete answer from a
// cut-off one.
func (r streamResult) emitFinish(onMessage func(upstream.SSEMessage)) {
	if onMessage == nil {
		return
	}
	event := map[string]interface{}{"finishReason": r.FinishReason()}
	if len(r.Usage) > 0 {
		event["usage"] = r.Usage
	}
	onMessage(upstream.SSEMessage{Type: "model.finish", Event: event})
}

var toolCallSequence atomic.Uint64

// NewToolCallID mints a local tool-call id for upstream deltas that omit one.
func NewToolCallID() string {
	return fmt.Sprintf("toolu_%d_%d", time.Now().UnixNano(), toolCallSequence.Add(1))
}

// toolCallAccumulator rebuilds tool calls from streamed deltas, where the name
// arrives in the first delta and the arguments are streamed afterwards.
//
// The upstream sometimes reuses one index for calls that are only distinguished
// by id. The order therefore stores call instances rather than indexes; the map
// only identifies which instance receives an id-less continuation delta.
type toolCallAccumulator struct {
	order []*toolCallState
	calls map[int]*toolCallState
}

type toolCallState struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Emitted   bool
}

const maxTextToolFallbackBytes = 2 << 20

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		calls: map[int]*toolCallState{},
	}
}

// add folds one delta into the call at index.
func (a *toolCallAccumulator) add(index int, id, name, args string) *toolCallState {
	trimmedID := strings.TrimSpace(id)
	state, ok := a.calls[index]
	if ok && (state.Emitted || (trimmedID != "" && state.ID != "" && state.ID != trimmedID)) {
		// A closed call or a new id at the same index starts another call
		// instance. Keeping the old pointer in order preserves parallel calls.
		state = nil
		ok = false
	}
	if !ok {
		state = &toolCallState{}
		a.calls[index] = state
		a.order = append(a.order, state)
	}
	if trimmedID != "" {
		state.ID = trimmedID
	}
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		state.Name = trimmed
	}
	state.Arguments.WriteString(args)
	return state
}

// pending returns the not-yet-emitted calls in stream order.
func (a *toolCallAccumulator) pending() []*toolCallState {
	out := make([]*toolCallState, 0, len(a.order))
	for _, state := range a.order {
		if state == nil || state.Emitted || state.Name == "" {
			continue
		}
		out = append(out, state)
	}
	return out
}

func (a *toolCallAccumulator) completeAll() []*toolCallState {
	pending := a.pending()
	for _, state := range pending {
		state.Emitted = true
	}
	return pending
}

// sseFrame is one accumulated SSE event.
type sseFrame struct {
	event string
	data  string
}

// readSSE frames the upstream stream. A `data:` line continues the current
// frame until a blank line closes it, and an `event:` line names it.
func readSSE(reader io.Reader, fn func(sseFrame) bool) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var event, data strings.Builder
	flush := func() bool {
		if event.Len() == 0 && data.Len() == 0 {
			return true
		}
		frame := sseFrame{event: event.String(), data: data.String()}
		event.Reset()
		data.Reset()
		return fn(frame)
	}

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		switch {
		case line == "":
			if !flush() {
				return nil
			}
		case strings.HasPrefix(line, ":"):
			// Comment / keep-alive.
		case strings.HasPrefix(line, "event:"):
			if event.Len() > 0 {
				event.WriteByte(' ')
			}
			event.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		case strings.HasPrefix(line, "data:"):
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(value)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A trailing frame without its blank-line terminator still counts.
	flush()
	return nil
}

// consumeStreamWithTools also recognizes the text fallback emitted by some
// Qoder models: `Tool calls: [...]`. Text is buffered only while it can still be
// that exact prefix; normal answers continue streaming as soon as they diverge.
func consumeStreamWithTools(body io.Reader, toolsEnabled bool, onMessage func(upstream.SSEMessage)) (streamResult, error) {
	result := streamResult{}
	tools := newToolCallAccumulator()
	var pendingText strings.Builder
	bufferingToolText := toolsEnabled
	// Leading whitespace is only relevant while deciding whether the stream can
	// still begin with "Tool calls:". Tracking this offset incrementally avoids
	// rescanning an ever-growing whitespace prefix on every tiny SSE delta.
	toolPrefixOffset := 0
	sawNativeTools := false

	emitText := func(text string) {
		if text == "" {
			return
		}
		result.SawMeaningfulEvent = true
		if onMessage != nil {
			onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]interface{}{
				"delta": text,
			}})
		}
	}

	flushPendingText := func() {
		if pendingText.Len() == 0 {
			return
		}
		emitText(pendingText.String())
		pendingText.Reset()
	}

	emitTools := func() {
		for _, state := range tools.completeAll() {
			result.SawMeaningfulEvent = true
			result.ToolCallCount++
			if onMessage == nil {
				continue
			}
			id := state.ID
			if id == "" {
				id = NewToolCallID()
			}
			onMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]interface{}{
				"toolCallId": id,
				"toolName":   state.Name,
				"input":      util.NormalizeToolInput(state.Arguments.String()),
			}})
		}
	}

	sawFinish := false
	var streamErr error
	var rateLimitText strings.Builder
	checkingRateLimitText := true

	readErr := readSSE(body, func(frame sseFrame) bool {
		if strings.EqualFold(strings.TrimSpace(frame.event), "finish") {
			sawFinish = true
			return false
		}
		if strings.EqualFold(strings.TrimSpace(frame.event), "error") {
			if streamErr == nil {
				streamErr = fmt.Errorf("qoder stream reported an error event")
			}
			return false
		}

		payload := strings.TrimSpace(frame.data)
		if payload == "" {
			return true
		}
		if payload == "[DONE]" {
			// Qoder may send usage or an error after this marker.
			return true
		}

		var envelope streamEnvelope
		if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
			streamErr = fmt.Errorf("qoder stream protocol error: invalid envelope: %w", err)
			return false
		}
		if envelope.StatusCodeValue != 0 && envelope.StatusCodeValue != http.StatusOK {
			var failure struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(envelope.Body), &failure)
			code := envelopeCode([]byte(envelope.Body))
			detail := strings.TrimSpace(failure.Message)
			if extracted := extractBodyMessage([]byte(envelope.Body)); extracted != "" {
				detail = extracted
			}
			if detail == "" {
				detail = fmt.Sprintf("upstream status %d", envelope.StatusCodeValue)
			}
			switch {
			case code == busyCode || sharedQueueRefusal(detail, envelope.Body):
				// The business code is the usual marker, but the same payload also
				// reaches here under an envelope the code reader cannot unwrap. It
				// must still classify as busy: read as a credential rejection it
				// parks a working account, and it loses the wait the upstream asked
				// for, so the retry comes back before the queue has cleared.
				busyErr := fmt.Errorf("%w: %s", ErrBusy, detail)
				streamErr = &attemptStreamError{err: busyErr, busy: true, retryable: true, wait: busyWait("", []byte(envelope.Body))}
			case isDuplicateRequest(detail, envelope.Body):
				// Replaying the same signed body/request id cannot repair an
				// idempotency conflict; it only creates a retry storm.
				streamErr = fmt.Errorf("qoder duplicate request")
			case hasAgentLimitReset(detail, envelope.Body):
				// This business payload is an allowance deadline carried under
				// 401, not a credential rejection. Refreshing the token is both
				// useless and harmful (it may rotate a still-valid credential).
				streamErr = &agentLimitError{resetAt: agentLimitResetAt(detail, envelope.Body)}
			case DetectNoEntitlement(detail, envelope.Body):
				// The credential was accepted; the account simply has no plan or
				// allowance for this model. This must not be classified as an
				// authentication failure, or a working account is retired.
				streamErr = entitlementError(envelope.Body)
			case IsContentPolicy(envelope.Body):
				// The upstream safety review refused the input. It is not a
				// capacity problem and not a credential problem: the client gets
				// a 400 straight away, and no account is marked rate-limited.
				streamErr = contentPolicyError(detail)
			case IsClientFault(envelope.Body):
				streamErr = fmt.Errorf("%w: %s", ErrClientFault, detail)
			case envelope.StatusCodeValue == http.StatusUnauthorized || envelope.StatusCodeValue == http.StatusForbidden:
				streamErr = fmt.Errorf("%w: %s", errUpstreamUnauthorized, detail)
			case IsTransientUpstreamStatus(envelope.StatusCodeValue, envelope.Body):
				// The gateway's own provider fault. It is worth one bounded
				// retry on this account, and it must not be read as a
				// credential rejection.
				streamErr = &attemptStreamError{err: transientError(fmt.Sprintf("qoder upstream error: status=%d, %s", envelope.StatusCodeValue, detail)), retryable: true}
			default:
				streamErr = fmt.Errorf("qoder upstream error: status=%d, %s", envelope.StatusCodeValue, detail)
			}
			return false
		}
		if strings.TrimSpace(envelope.Body) == "" {
			return true
		}
		if strings.TrimSpace(envelope.Body) == "[DONE]" {
			return true
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(envelope.Body), &chunk); err != nil {
			streamErr = fmt.Errorf("qoder stream protocol error: invalid body: %w", err)
			return false
		}
		if chunk.Error != nil && strings.TrimSpace(chunk.Error.Message) != "" {
			if stringOfCode(chunk.Error.Code) == busyCode || envelopeCode([]byte(chunk.Error.Message)) == busyCode {
				busyErr := fmt.Errorf("%w: %s", ErrBusy, chunk.Error.Message)
				streamErr = &attemptStreamError{err: busyErr, busy: true, retryable: true, wait: busyWait("", []byte(chunk.Error.Message))}
			} else if IsContentPolicy(chunk.Error.Message) {
				// A safety refusal is a verdict about the input. Replaying it
				// only sends the same rejected content again, and marking the
				// account rate-limited is what empties a healthy pool.
				streamErr = contentPolicyError(chunk.Error.Message)
			} else if IsClientFault(chunk.Error.Message) {
				// A request the upstream rejected on its own merits.
				streamErr = fmt.Errorf("%w: %s", ErrClientFault, chunk.Error.Message)
			} else {
				streamErr = fmt.Errorf("qoder stream error: %s", chunk.Error.Message)
			}
			// The frame said it failed, so the stream is over: anything after
			// it is not answer data, and reading on would only turn the error
			// into a truncation.
			return false
		}
		if chunk.Usage != nil {
			if usage := normalizeUsage(chunk.Usage); len(usage) > 0 {
				result.Usage = usage
				result.SawMeaningfulEvent = true
				if onMessage != nil {
					onMessage(upstream.SSEMessage{Type: "model.tokens-used", Event: usage})
				}
			}
		}
		if len(chunk.Choices) == 0 {
			return true
		}
		choice := chunk.Choices[0]
		delta := choice.Delta

		if delta.ReasoningContent != "" {
			result.SawMeaningfulEvent = true
			if onMessage != nil {
				if result.ThinkingSignature == "" {
					result.ThinkingSignature = newThinkingSignature()
				}
				onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{
					"delta":     delta.ReasoningContent,
					"signature": result.ThinkingSignature,
				}})
			}
		}
		if delta.Content != "" {
			// Buffer the beginning of text long enough to recognize Qoder's
			// account-pool throttle even when the sentinel spans SSE deltas. Once
			// the prefix can no longer become that message, release it normally.
			if checkingRateLimitText {
				rateLimitText.WriteString(delta.Content)
				candidate := rateLimitText.String()
				if isModelRateLimitText(candidate) {
					streamErr = fmt.Errorf("%w: %s", ErrModelRateLimited, strings.TrimSpace(candidate))
					return false
				}
				if !isPotentialModelRateLimitText(candidate) {
					checkingRateLimitText = false
					delta.Content = candidate
					rateLimitText.Reset()
				} else {
					// Only text is deferred. This choice may also carry native
					// tool deltas and its finish reason, which must still be read.
					delta.Content = ""
				}
			}
		}
		reason := strings.TrimSpace(choice.FinishReason)
		if checkingRateLimitText && (len(delta.ToolCalls) > 0 || (reason != "" && reason != "null")) {
			// A tool boundary or completed choice resolves an incomplete throttle
			// prefix as prose. Release it before tools so it cannot appear after
			// a tool call emitted by this choice's finish reason.
			checkingRateLimitText = false
			emitText(rateLimitText.String())
			rateLimitText.Reset()
		}
		if delta.Content != "" {
			if !bufferingToolText || sawNativeTools {
				emitText(delta.Content)
			} else {
				pendingText.WriteString(delta.Content)
				candidate := pendingText.String()
				for toolPrefixOffset < len(candidate) {
					switch candidate[toolPrefixOffset] {
					case ' ', '\t', '\r', '\n':
						toolPrefixOffset++
					default:
						goto prefixReady
					}
				}
			prefixReady:
				if pendingText.Len() > maxTextToolFallbackBytes || !isPotentialTextToolCallAt(candidate, toolPrefixOffset) {
					bufferingToolText = false
					flushPendingText()
				}
			}
		}
		for _, call := range delta.ToolCalls {
			result.SawMeaningfulEvent = true
			if !sawNativeTools {
				sawNativeTools = true
				// A textual prefix followed by native tool deltas is a duplicate
				// representation, not assistant prose.
				pendingText.Reset()
			}
			tools.add(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
		}
		if reason != "" && reason != "null" {
			result.FinishReasonValue = reason
			// Arguments can span several deltas, so calls are only emitted once
			// the choice says it is done.
			emitTools()
		}
		return true
	})

	if readErr != nil {
		return result, fmt.Errorf("read qoder stream: %w", readErr)
	}
	if streamErr != nil {
		return result, streamErr
	}
	if checkingRateLimitText && rateLimitText.Len() > 0 {
		emitText(rateLimitText.String())
	}
	if toolsEnabled && !sawNativeTools && pendingText.Len() > 0 {
		if parsed := parseTextToolCalls(pendingText.String()); len(parsed) > 0 {
			pendingText.Reset()
			for index, call := range parsed {
				tools.add(index, call.ID, call.Name, call.Arguments)
			}
			result.SawMeaningfulEvent = true
		} else {
			flushPendingText()
		}
	}
	emitTools()
	if !sawFinish {
		// An EOF without a finish event means the connection was cut
		// mid-answer. Reporting success here would truncate silently.
		return result, ErrStreamTruncated
	}
	return result, nil
}

// ErrModelRateLimited marks Qoder's text-form model/account-pool throttle.
// It is intentionally distinct from a credential failure: other models on the
// same account remain usable while this model cools down.
var ErrModelRateLimited = fmt.Errorf("qoder model rate limited")

func isPotentialModelRateLimitText(text string) bool {
	candidate := strings.ToLower(strings.TrimSpace(text))
	if candidate == "" {
		return true
	}
	for _, sentinel := range []string{
		"the available upstream accounts are rate-limited",
		"the available upstream accounts are rate limited",
		"available upstream accounts are rate-limited",
		"available upstream accounts are rate limited",
	} {
		if strings.HasPrefix(sentinel, candidate) || strings.Contains(candidate, sentinel) {
			return true
		}
	}
	return false
}

func isModelRateLimitText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(lower, "available upstream accounts are rate-limited") ||
		strings.Contains(lower, "available upstream accounts are rate limited")
}

type textToolCall struct {
	ID        string
	Name      string
	Arguments string
}

func isPotentialTextToolCallAt(text string, offset int) bool {
	if offset >= len(text) {
		return true
	}
	candidate := text[offset:]
	const prefix = "Tool calls:"
	return strings.HasPrefix(prefix, candidate) || strings.HasPrefix(candidate, prefix)
}

func parseTextToolCalls(text string) []textToolCall {
	const prefix = "Tool calls:"
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, prefix) {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, prefix))
	if strings.HasPrefix(payload, "```") && strings.HasSuffix(payload, "```") {
		if newline := strings.IndexByte(payload, '\n'); newline >= 0 {
			payload = strings.TrimSpace(payload[newline+1 : len(payload)-3])
		}
	}
	if !strings.HasPrefix(payload, "[") {
		return nil
	}

	var raw []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil || len(raw) == 0 {
		return nil
	}
	if len(raw) > 128 {
		raw = raw[:128]
	}
	out := make([]textToolCall, 0, len(raw))
	for _, call := range raw {
		name := strings.TrimSpace(call.Function.Name)
		if name == "" {
			continue
		}
		arguments := "{}"
		if len(call.Function.Arguments) > 0 && string(call.Function.Arguments) != "null" {
			var encoded string
			if json.Unmarshal(call.Function.Arguments, &encoded) == nil {
				arguments = encoded
			} else if json.Valid(call.Function.Arguments) {
				arguments = string(call.Function.Arguments)
			}
		}
		out = append(out, textToolCall{ID: strings.TrimSpace(call.ID), Name: name, Arguments: arguments})
	}
	return out
}

// busyCode is the gateway's queue/concurrency refusal. It arrives as a business
// code, sometimes under a 401 status, so it must be classified before the status
// is trusted.
const busyCode = "10605"

// errUpstreamUnauthorized marks an authentication failure that a token refresh
// can plausibly fix.
var errUpstreamUnauthorized = fmt.Errorf("qoder upstream rejected the credential")

type agentLimitError struct {
	resetAt time.Time
}

func (e *agentLimitError) Error() string {
	if e != nil && !e.resetAt.IsZero() {
		return "qoder agent limit reached; resets at " + e.resetAt.UTC().Format(time.RFC3339)
	}
	return "qoder agent limit reached"
}

func isDuplicateRequest(values ...string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), "duplicate request") {
			return true
		}
	}
	return false
}

func hasAgentLimitReset(values ...string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), "agentlimitresettime") {
			return true
		}
	}
	return false
}

func agentLimitResetAt(values ...string) time.Time {
	for _, value := range values {
		if parsed := agentLimitResetAtDepth(strings.TrimSpace(value), 0); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

func agentLimitResetAtDepth(value string, depth int) time.Time {
	if value == "" || depth > 3 {
		return time.Time{}
	}
	var payload struct {
		ResetAt int64  `json:"agentLimitResetTime"`
		Message string `json:"message"`
		Body    string `json:"body"`
	}
	if json.Unmarshal([]byte(value), &payload) != nil {
		return time.Time{}
	}
	if payload.ResetAt > 0 {
		if payload.ResetAt < 100000000000 {
			return time.Unix(payload.ResetAt, 0)
		}
		return time.UnixMilli(payload.ResetAt)
	}
	for _, nested := range []string{payload.Message, payload.Body} {
		if parsed := agentLimitResetAtDepth(strings.TrimSpace(nested), depth+1); !parsed.IsZero() {
			return parsed
		}
	}
	return time.Time{}
}

func stringOfCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return strings.TrimSpace(number.String())
	}
	return ""
}

func newThinkingSignature() string {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return "qoder-v1:" + base64.RawURLEncoding.EncodeToString(raw[:])
	}
	return fmt.Sprintf("qoder-v1:%d", time.Now().UnixNano())
}

// normalizeUsage maps the upstream usage object onto the key names the shared
// stream handler consumes, keeping the credit fields under their own names so
// a cost panel can read them.
func normalizeUsage(usage *streamUsage) map[string]interface{} {
	if usage == nil {
		return nil
	}
	out := make(map[string]interface{}, 8)
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		out["inputTokens"] = usage.PromptTokens
		out["input_tokens"] = usage.PromptTokens
		out["outputTokens"] = usage.CompletionTokens
		out["output_tokens"] = usage.CompletionTokens
	}
	if usage.TotalTokens > 0 {
		out["totalTokens"] = usage.TotalTokens
	}
	if usage.PromptTokensDetails != nil {
		if usage.PromptTokensDetails.CachedTokens > 0 {
			out["cacheReadTokens"] = usage.PromptTokensDetails.CachedTokens
			out["cache_read_tokens"] = usage.PromptTokensDetails.CachedTokens
		}
		if usage.PromptTokensDetails.CacheableTokens > 0 {
			out["cacheWriteTokens"] = usage.PromptTokensDetails.CacheableTokens
			out["cache_creation_input_tokens"] = usage.PromptTokensDetails.CacheableTokens
		}
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		out["reasoningTokens"] = usage.CompletionTokensDetails.ReasoningTokens
	}
	if usage.Credits != nil {
		out["credits"] = *usage.Credits
	}
	if usage.OriginalCredits != nil {
		out["original_credits"] = *usage.OriginalCredits
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractBodyMessage reads the human-readable message out of an error envelope.
func extractBodyMessage(raw []byte) string {
	var envelope struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &envelope); err != nil {
		return ""
	}
	var failure struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(envelope.Body), &failure); err != nil {
		return ""
	}
	return strings.TrimSpace(failure.Message)
}
