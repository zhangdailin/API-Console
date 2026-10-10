package qoder

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"encoding/json"

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
//
// The same channel also carries advisory control frames, prefixed "[KIND]#",
// which are not chunks and must not be parsed as one.

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
	Billable        *bool    `json:"billable"`
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
	// ControlFrames counts the advisory frames the gateway multiplexed into the
	// chunk channel, keyed by kind. They carry no model output, but a quota
	// notice explains queue refusals that are otherwise unattributable.
	ControlFrames map[string]int
	// QuotaNotice is the account's own quota verdict, when the upstream
	// multiplexed one into the stream. A complete answer can still carry a
	// quota_exceeded notice; it is the earliest authoritative statement of when
	// the window resets, and it is what lets the pool stop trusting a snapshot
	// taken before that reset.
	QuotaNotice *QuotaNotice
}

// QuotaNotice is the advisory quota frame the upstream multiplexes into the
// chunk channel. The gateway used to count these frames and discard the
// payload, so an account the upstream had already told us was spent kept its
// stale "has credits" snapshot until the next periodic sync — which is how a
// request stayed routable onto an account whose window had already closed.
type QuotaNotice struct {
	// Kind is the control-frame kind that carried the notice ("NOTIFICATIONS").
	Kind string
	// Exhausted is true for a quota_exceeded notice: the upstream is saying the
	// account's allowance for this window is spent.
	Exhausted bool
	// HighestTier reports whether the account already holds the highest plan the
	// upstream offers, which is what makes an upgrade prompt pointless.
	HighestTier bool
	// NextResetAt is when the window reopens. Zero when the upstream omitted it.
	NextResetAt time.Time
	// UpgradeURL and PricingURL are the upstream's own links for the notice. They
	// are kept for the log and the account view, never for the client's answer.
	UpgradeURL string
	PricingURL string
}

// notificationFrame is the payload shape of a NOTIFICATIONS control frame:
//
//	{"notifications":[{"notificationType":"quota_exceeded","isHighestTier":false,
//	  "extras":{"nextResetAt":1759…,"pricingUrl":"…","upgradeUrl":"…"}}]}
type notificationFrame struct {
	Notifications []struct {
		NotificationType string `json:"notificationType"`
		IsHighestTier    bool   `json:"isHighestTier"`
		Extras           struct {
			NextResetAt int64  `json:"nextResetAt"`
			UpgradeURL  string `json:"upgradeUrl"`
			PricingURL  string `json:"pricingUrl"`
		} `json:"extras"`
	} `json:"notifications"`
}

// parseQuotaNotice extracts the quota verdict from an advisory frame. It returns
// nil for every other kind, for a malformed payload, and for a notifications
// frame that carries no quota_exceeded entry.
func parseQuotaNotice(kind, payload string) *QuotaNotice {
	if !strings.EqualFold(strings.TrimSpace(kind), "NOTIFICATIONS") {
		return nil
	}
	// The frame is sometimes followed by its own terminator, so a trailing '#'
	// survives the split. Trim it before decoding rather than treating the whole
	// payload as malformed.
	payload = strings.TrimSpace(payload)
	payload = strings.TrimRight(payload, "#")
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	var frame notificationFrame
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		return nil
	}
	for _, n := range frame.Notifications {
		if !strings.EqualFold(strings.TrimSpace(n.NotificationType), "quota_exceeded") {
			continue
		}
		notice := &QuotaNotice{
			Kind:        strings.ToUpper(strings.TrimSpace(kind)),
			Exhausted:   true,
			HighestTier: n.IsHighestTier,
			UpgradeURL:  strings.TrimSpace(n.Extras.UpgradeURL),
			PricingURL:  strings.TrimSpace(n.Extras.PricingURL),
		}
		if n.Extras.NextResetAt > 0 {
			notice.NextResetAt = time.Unix(normalizeMillis(n.Extras.NextResetAt), 0)
		}
		return notice
	}
	return nil
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

// NewToolCallID mints a local tool-call id for upstream deltas that omit one.
func NewToolCallID() string { return util.NewToolCallID() }

// sseFrame is one accumulated SSE event.
type sseFrame struct {
	event string
	data  string
}

// readSSE frames the upstream stream. A `data:` line continues the current
// frame until a blank line closes it, and an `event:` line names it.
// bodySnippet renders a bounded single-line view of an envelope body that could
// not be parsed. A refusal the upstream explained must not reach the operator as
// a bare "unsupported stream format".
func bodySnippet(body string) string {
	const limit = 400
	compact := strings.Join(strings.Fields(body), " ")
	if compact == "" {
		return "<empty>"
	}
	if len(compact) > limit {
		return compact[:limit] + "..."
	}
	return compact
}

// splitControlFrame reports whether an envelope body is an advisory control
// frame rather than a chat chunk. The gateway prefixes those with "[KIND]#" and
// multiplexes them into the same channel as model output.
func splitControlFrame(body string) (kind, payload string, ok bool) {
	trimmed := strings.TrimSpace(body)
	if !strings.HasPrefix(trimmed, "[") {
		return "", "", false
	}
	end := strings.Index(trimmed, "]#")
	if end < 0 {
		return "", "", false
	}
	return trimmed[1:end], strings.TrimSpace(trimmed[end+2:]), true
}

func readSSE(reader io.Reader, fn func(sseFrame) bool) error {
	scanner := bufio.NewScanner(reader)
	buffer := util.AcquireStreamBuffer()
	defer util.ReleaseStreamBuffer(buffer)
	scanner.Buffer(buffer[:], 16*1024*1024)

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

func consumeStreamObserved(body io.Reader, _ bool, onMessage func(upstream.SSEMessage), onFrame func()) (streamResult, error) {
	result := streamResult{}
	tools := util.NewToolCallAccumulator()
	emitText := func(text string) { upstream.EmitTextDelta(onMessage, text, &result.SawMeaningfulEvent) }

	emitTools := func() {
		completed := tools.CompleteAll()
		for _, call := range completed {
			call.Arguments = normalizeCommandEscalation(call.Name, call.Arguments)
		}
		upstream.EmitToolCalls(onMessage, completed, &result.SawMeaningfulEvent, &result.ToolCallCount)
	}

	sawFinish := false
	var streamErr error
	var rateLimitText strings.Builder
	checkingRateLimitText := true

	readErr := readSSE(body, func(frame sseFrame) bool {
		if onFrame != nil {
			onFrame()
		}
		if strings.EqualFold(strings.TrimSpace(frame.event), "finish") {
			sawFinish = true
			var timings map[string]json.RawMessage
			if json.Unmarshal([]byte(frame.data), &timings) == nil {
				for _, key := range []string{"firstTokenDuration", "totalDuration", "serverDuration"} {
					if raw, ok := timings[key]; ok {
						var value int64
						if json.Unmarshal(raw, &value) == nil && value >= 0 {
							if result.Usage == nil {
								result.Usage = map[string]interface{}{}
							}
							result.Usage[key] = value
						}
					}
				}
			}
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
			case IsDailyCountExceeded(detail, envelope.Body):
				// The account's daily request count is spent. The credential is
				// valid and the 401/403 envelope must not be read as a rejection of
				// it: only another account or the next reset can serve this request.
				streamErr = &attemptStreamError{err: fmt.Errorf("%w: %s", ErrDailyCountExceeded, detail), retryable: true}
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
		// Advisory frames share the chunk channel. Parsing one as a chunk used
		// to abort an otherwise healthy reply: a quota_low notice reached the
		// client as "unsupported stream format".
		if kind, notice, ok := splitControlFrame(envelope.Body); ok {
			if result.ControlFrames == nil {
				result.ControlFrames = map[string]int{}
			}
			result.ControlFrames[kind]++
			if quotaNotice := parseQuotaNotice(kind, notice); quotaNotice != nil {
				// Keep the latest reading in the attempt's result; the caller
				// records it against the account once the attempt settles.
				result.QuotaNotice = quotaNotice
				slog.Warn("qoder quota notice in stream", "provider", "qoder",
					"kind", kind,
					"exhausted", quotaNotice.Exhausted,
					"highest_tier", quotaNotice.HighestTier,
					"next_reset_at", quotaNotice.NextResetAt.Format(time.RFC3339),
				)
			}
			slog.Warn("qoder control frame", "provider", "qoder", "kind", kind, "payload", bodySnippet(notice))
			return true
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(envelope.Body), &chunk); err != nil {
			// The upstream reports some refusals as a JSON array rather than a
			// chunk object. Reporting only "invalid body" hides the reason it
			// gave, and that reason is the one thing that makes the refusal
			// actionable.
			streamErr = fmt.Errorf("qoder stream protocol error: invalid body: %w (upstream body: %s)", err, bodySnippet(envelope.Body))
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
		upstream.ApplyStreamUsage(onMessage, normalizeUsage(chunk.Usage), &result.SawMeaningfulEvent, &result.Usage)
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
			emitText(delta.Content)
		}
		for _, call := range delta.ToolCalls {
			result.SawMeaningfulEvent = true
			tools.Add(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
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
	return slices.ContainsFunc(values, func(value string) bool { return strings.Contains(strings.ToLower(value), "duplicate request") })
}

func hasAgentLimitReset(values ...string) bool {
	return slices.ContainsFunc(values, func(value string) bool { return strings.Contains(strings.ToLower(value), "agentlimitresettime") })
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

func newThinkingSignature() string { return util.NewThinkingSignature("qoder-v1") }

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
			out["cacheable_tokens"] = usage.PromptTokensDetails.CacheableTokens
		}
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		out["reasoningTokens"] = usage.CompletionTokensDetails.ReasoningTokens
	}
	if usage.Billable != nil {
		out["billable"] = *usage.Billable
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
