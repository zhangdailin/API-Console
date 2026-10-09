package handler

import (
	"log/slog"
	"strings"
	"time"

	"orchids-api/internal/adapter"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/upstream"
)

func (h *streamHandler) finishResponse(stopReason string) {
	if stopReason == "tool_use" {
		h.mu.Lock()
		hasToolCalls := h.toolCallCount > 0 ||
			len(h.pendingToolCalls) > 0 ||
			len(h.toolCallEmitted) > 0
		h.mu.Unlock()
		if !hasToolCalls {
			stopReason = "end_turn"
		}
	}

	// A strict structured-output request is answered only when the answer
	// matches the schema. The check happens before anything is committed: a
	// stream that has not opened yet can still fail with a real status, and a
	// non-stream response has committed nothing at all. Delivering an answer
	// the caller's parser will reject, with a 200, is worse than an error it
	// can retry or relax.
	if err := h.validateStructuredAnswer(); err != nil {
		slog.Warn("Structured output did not match the requested schema", "error", err)
		h.reportRequestFailure("Reporting that the answer did not match the requested schema",
			"schema_mismatch", err.Error(), 0)
		return
	}

	if !h.hasVisibleOutput() {
		h.mu.Lock()
		fallback := h.emptyOutputFallback
		h.mu.Unlock()
		if fallback != "" {
			h.mu.Lock()
			h.useUpstreamUsage = false
			h.outputTokens = 0
			h.outputEstimator.Reset()
			h.mu.Unlock()
			h.handleMessage(upstream.SSEMessage{
				Type:  "model.text-delta",
				Event: map[string]interface{}{"delta": fallback},
			})
		}
	}

	// The opening frame is deferred until there is something to send, so a
	// response that is finishing without any content -- or any stream that never
	// produced one -- must open here, while the response is still writable. Doing
	// it after hasReturn is set would be dropped by the writers below.
	h.mu.Lock()
	opened := h.ensureMessageStartLocked()
	h.mu.Unlock()
	if h.isStream && !opened {
		// Either the stream is already open, or an error owns the response.
		h.mu.Lock()
		alreadyOpen := h.messageStartWritten
		h.mu.Unlock()
		if !alreadyOpen {
			return
		}
	}

	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	h.hasReturn = true
	h.returned.Store(true)
	h.finalStopReason = stopReason
	h.mu.Unlock()

	if h.isStream {
		var blockStopData []byte
		h.mu.Lock()
		if stopData, ok := h.popActiveBlockStopDataLocked(); ok {
			blockStopData = stopData
		}
		h.mu.Unlock()
		if len(blockStopData) > 0 {
			h.writeFinalSSEBytes("content_block_stop", blockStopData)
		}
		h.flushPendingToolCalls()
		h.finalizeOutputTokens()
		h.mu.Lock()
		h.writeSSEMessageDeltaLocked(stopReason, h.outputTokens, true)
		h.mu.Unlock()

		stopData, err := marshalSSEMessageStopBytes()
		if err != nil {
			slog.Error("Failed to marshal message_stop", "error", err)
		} else {
			h.writeFinalSSEBytes("message_stop", stopData)
		}
	} else {
		h.flushPendingToolCalls()
		h.finalizeOutputTokens()
	}

	h.finalizeCompletion(stopReason)
}

func (h *streamHandler) finalizeCompletion(stopReason string) {
	h.mu.Lock()
	if h.completionLogged {
		h.mu.Unlock()
		return
	}
	h.completionLogged = true
	h.mu.Unlock()

	// The summary is written outside the lock: LogSummary and the debug line only
	// read counters, and the guard above already made this run once per round.
	h.logger.LogSummary(h.inputTokens, h.outputTokens, time.Since(h.startTime), stopReason)
	slog.Debug("Request completed", "input_tokens", h.inputTokens, "output_tokens", h.outputTokens, "duration", time.Since(h.startTime))
}

func (h *streamHandler) markWriteError(event string, err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.markWriteErrorLocked(event, err)
}

func (h *streamHandler) markWriteErrorLocked(event string, err error) {
	if err == nil {
		return
	}
	// finishResponse claims hasReturn before writing its terminal frames. A write
	// can therefore fail after the response is already terminal; do not mistake
	// that claimed state for a successful write and hide the failure.
	alreadyFailed := h.requestFailed
	h.hasReturn = true
	h.requestFailed = true
	h.returned.Store(true)
	h.finalStopReason = "write_error"
	if h.cancelUpstream != nil {
		h.cancelUpstream()
	}
	middleware.MarkStreamFailure(h.w)
	if !alreadyFailed {
		slog.Warn("Response write failed", "event", event, "error", err)
	}
}

func (h *streamHandler) forceFinishIfMissing() {
	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	hasToolCalls := h.toolCallCount > 0 ||
		len(h.pendingToolCalls) > 0 ||
		len(h.toolCallEmitted) > 0
	h.mu.Unlock()

	stopReason := "end_turn"
	if hasToolCalls {
		stopReason = "tool_use"
	}
	slog.Warn("Upstream stream ended without explicit stop marker; forcing response finish", "stop_reason", stopReason)
	h.finishResponse(stopReason)
}

func (h *streamHandler) hasAnyOutput() bool {
	h.mu.Lock()
	has := h.hasReasoningOutput || h.useUpstreamUsage
	h.mu.Unlock()
	return has || h.hasVisibleOutput()
}

func (h *streamHandler) hasVisibleOutput() bool {
	h.mu.Lock()
	has := h.hasTextOutput ||
		h.toolCallCount > 0 ||
		len(h.pendingToolCalls) > 0 ||
		len(h.toolCallEmitted) > 0 ||
		h.responseText.Len() > 0
	h.mu.Unlock()
	return has
}

// reportRequestFailure tells the client the request failed without producing an
// answer.
//
// A completion whose content is "the available upstream accounts have exhausted
// their quota" is indistinguishable from an answer: clients persist it, agents
// feed it back as context, and nothing downstream retries or alerts on it. The
// failure belongs to the gateway, so it is reported as one.
//
// A non-streaming request has committed nothing by this point, so the report is an
// HTTP error carrying the status StatusForCategory assigns to the category. A
// stream has already sent its message_start — the status is 200 and cannot be
// revisited — so its report has to stay in band, but it carries the same
// operator-facing message rather than a re-classified one.
func (h *streamHandler) terminalState() (returned, failed bool) {
	if h == nil {
		return false, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hasReturn, h.requestFailed
}

func (h *streamHandler) reportRequestFailure(logMsg, category, message string, retryAfter time.Duration) {
	if h == nil || h.w == nil {
		return
	}
	if logutil.VerboseDiagnosticsEnabled() {
		slog.Debug(logMsg, "category", category, "message", message)
	}
	// An in-band error is only forced once something has actually been sent: the
	// opening frame is written lazily now, so a stream that failed before any
	// content still has a status left to set. Reporting in band there is what made
	// a retryable condition look like a truncated answer.
	if h.isStream && h.hasCommitted() {
		h.writeStreamError(category, message)
		if logutil.VerboseDiagnosticsEnabled() {
			slog.Debug(logMsg+" (in band: the status is already committed)", "category", category)
		}
		return
	}
	h.mu.Lock()
	if h.hasReturn {
		h.mu.Unlock()
		return
	}
	// Claim the response under the lock so a concurrent finisher cannot also write
	// a body; finishResponse then returns early and leaves the error in place.
	h.hasReturn = true
	h.requestFailed = true
	h.returned.Store(true)
	h.mu.Unlock()
	// retryAfter publishes the upstream's own "come back in" hint. A caller that
	// just waited out several windows deserves to know when the answer might
	// change instead of retrying blindly.
	apperrors.NewWithRetryAfter(category, message, apperrors.StatusForCategory(category), retryAfter).WriteResponse(h.w)
}

// writeStreamError reports a failure inside a stream that has already started.
//
// The status cannot be changed — message_start is on the wire — so the report goes
// in band, in the shape each protocol defines for it. The stream is then closed
// here rather than by finishResponse, so a normal stop does not follow the error
// and a client cannot mistake the failure for a completed answer.
func (h *streamHandler) writeStreamError(category, message string) {
	if h == nil || h.w == nil || !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hasReturn {
		return
	}
	h.hasReturn = true
	h.requestFailed = true
	h.returned.Store(true)
	middleware.MarkStreamFailure(h.w)

	if h.responseFormat == adapter.FormatOpenAI {
		data, err := marshalOpenAIErrorBytes(category, message)
		if err != nil {
			slog.Error("Failed to marshal stream error", "error", err)
			return
		}
		if err := writeOpenAIFrame(h.w, data); err != nil {
			slog.Warn("Failed to write stream error", "error", err)
			return
		}
		if _, err := h.w.Write(sseDoneLineBytes); err != nil {
			slog.Warn("Failed to terminate stream after an error", "error", err)
		}
	} else {
		data, err := marshalAnthropicErrorBytes(category, message)
		if err != nil {
			slog.Error("Failed to marshal stream error", "error", err)
			return
		}
		if err := writeSSEFrameBytes(h.w, "error", data); err != nil {
			slog.Warn("Failed to write stream error", "error", err)
		}
	}
	if h.flusher != nil {
		h.flusher.Flush()
	}
}

func (h *streamHandler) InjectNoAvailableAccountError(lastErr string, selectErr error) {
	// One rule for "no account could take this request", shared with the initial
	// selection in Handler.HandleMessages: the two entrances used to answer the
	// same condition differently, which is how a cooling pool reached one caller
	// as a retryable 429 and another as a 503 server fault.
	out := apperrors.ClassifyPoolExhaustion(selectErr, lastErr)
	if out.Empty() {
		// Nothing named the cause, so the retry-exhausted wording and whatever the
		// upstream error implies stand.
		out = apperrors.PoolExhaustion{
			Category: apperrors.ClassifyUpstreamError(lastErr).Category,
			Message:  apperrors.PoolRetriesExhaustedMessage,
		}
	}
	// The selector error and the last upstream error are diagnostics. They go to
	// the log rather than into the response, which a client may show to a user and
	// which is now an error body rather than a completion.
	if selectErr != nil || strings.TrimSpace(lastErr) != "" {
		slog.Warn("Reporting that no account could serve the request", "select_error", selectErr, "last_error", lastErr)
	}
	h.reportRequestFailure("Injecting no available account error to client", out.Category, out.Message, 0)
}
