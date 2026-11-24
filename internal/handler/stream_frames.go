package handler

import (
	"bytes"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"

	"orchids-api/internal/adapter"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/logutil"
)

func mapKeys(m map[string]any) []string {
	if m == nil {
		return nil
	}
	return slices.Collect(maps.Keys(m))
}

// CoalesceStreamFlush owns the batching timer, so signal it after every frame.
func (h *streamHandler) notifyStreamFlushLocked() {
	if h.flusher != nil {
		h.flushStreamLocked()
	}
}

func (h *streamHandler) flushStreamLocked() {
	if err := http.NewResponseController(h.w).Flush(); err != nil && err != http.ErrNotSupported {
		h.markWriteErrorLocked("flush", err)
	}
}

func (h *streamHandler) writeOpenAISSEBytes(event string, data []byte) (bool, error) {
	raw, ok := adapter.AppendOpenAIChunk(h.openAIChunkScratch[:0], h.msgID, h.startTime.Unix(), event, data)
	if !ok {
		return false, nil
	}
	h.openAIChunkScratch = raw[:0]
	if err := h.writeStreamFrameLocked("", raw); err != nil {
		return false, err
	}
	return true, nil
}

// One write per frame avoids repeated deadline updates and pipe rendezvous
// when Chat is translated into Responses. The buffer belongs to this handler
// and is reused only after Write returns; callers hold h.mu.
func (h *streamHandler) writeStreamFrameLocked(event string, data []byte) error {
	frame := httpserver.AppendSSEFrame(h.sseFrameScratch[:0], event, data)
	h.sseFrameScratch = frame[:0]
	n, err := (httpserver.DeadlineResponseWriter{ResponseWriter: h.w}).Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}

// emitSSEFrameLocked writes and flushes a serialized frame in the requested dialect.
func (h *streamHandler) emitSSEFrameLocked(event string, data []byte, final bool) {
	if !h.isStream || h.requestFailed {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSEBytes(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.notifyStreamFlushLocked()
		}
		if final && event == "message_stop" && !h.requestFailed {
			if _, err := (httpserver.DeadlineResponseWriter{ResponseWriter: h.w}).Write(sseDoneLineBytes); err != nil {
				h.markWriteErrorLocked(event, err)
				return
			}
			h.notifyStreamFlushLocked()
		}
		return
	}
	if err := h.writeStreamFrameLocked(event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.notifyStreamFlushLocked()
	if h.config != nil && h.config.DebugEnabled && h.config.DebugLogSSE {
		h.logger.LogOutputSSE(event, string(data))
	}
}

func (h *streamHandler) writeFinalSSEBytes(event string, data []byte) {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.emitSSEFrameLocked(event, data, true)
}

func (h *streamHandler) writeSSEBytesLocked(event string, data []byte) {
	if !h.isStream || h.hasReturn {
		return
	}
	// Every event except the opening frame itself needs the stream open first, so
	// the client never sees a content frame before message_start.
	if event != "message_start" && !h.ensureMessageStartLocked() {
		return
	}
	h.emitSSEFrameLocked(event, data, false)
	if logutil.VerboseDiagnosticsEnabled() {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
}

func (h *streamHandler) writeEncodedSSELocked(event string, raw []byte, err error, final bool) {
	if err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.emitSSEFrameLocked(event, raw, true)
		return
	}
	h.writeSSEBytesLocked(event, raw)
}

func (h *streamHandler) writeSSEContentBlockStartToolUseLocked(index int, id, name string, final bool) {
	raw, err := appendSSEContentBlockStartToolUse(h.ssePayloadScratch[:0], index, id, name)
	h.writeEncodedSSELocked("content_block_start", raw, err, final)
}

func (h *streamHandler) writeSSEContentBlockStartTextLocked(index int) {
	raw, err := appendSSEContentBlockStartText(h.ssePayloadScratch[:0], index)
	h.writeEncodedSSELocked("content_block_start", raw, err, false)
}

func (h *streamHandler) writeSSEContentBlockDeltaInputJSONLocked(index int, partialJSON string, final bool) {
	raw, err := appendSSEContentBlockDeltaInputJSON(h.ssePayloadScratch[:0], index, partialJSON)
	h.writeEncodedSSELocked("content_block_delta", raw, err, final)
}

func (h *streamHandler) writeSSEContentBlockDeltaTextLocked(index int, text string) {
	raw, err := appendSSEContentBlockDeltaText(h.ssePayloadScratch[:0], index, text)
	h.writeEncodedSSELocked("content_block_delta", raw, err, false)
}

func (h *streamHandler) writeSSEContentBlockDeltaThinkingLocked(index int, thinking string) {
	raw, err := appendSSEContentBlockDeltaThinking(h.ssePayloadScratch[:0], index, thinking)
	h.writeEncodedSSELocked("content_block_delta", raw, err, false)
}

func (h *streamHandler) writeSSEContentBlockStopLocked(index int, final bool) {
	raw, err := appendSSEContentBlockStop(h.ssePayloadScratch[:0], index)
	h.writeEncodedSSELocked("content_block_stop", raw, err, final)
}

func (h *streamHandler) writeSSEMessageDeltaLocked(stopReason string, outputTokens int) {
	raw, err := appendSSEMessageDelta(h.ssePayloadScratch[:0], stopReason, outputTokens)
	if err == nil && h.exclusiveUsage.active {
		// The explicit Chat dialect carries canonical inclusive input without
		// reinterpreting a partial native Messages projection as a full total.
		raw = raw[:bytes.LastIndex(raw, []byte(`"usage":`))+len(`"usage":`)]
		raw, err = h.appendExclusiveWireUsage(raw, h.canonicalWireUsage || h.responseFormat == adapter.FormatOpenAI)
		raw = append(raw, '}')
	} else if err == nil && h.useUpstreamUsage && (h.canonicalWireUsage || h.responseFormat == adapter.FormatOpenAI) {
		raw = raw[:bytes.LastIndex(raw, []byte(`"usage":`))+len(`"usage":`)]
		raw = appendCanonicalWireUsage(raw, h.inputTokens, h.outputTokens, h.cachedInputTokens, h.cacheWriteTokens)
		raw = append(raw, '}')
	} else if err == nil && h.useUpstreamUsage {
		// The opening input count may be an estimate. Preserve the authoritative
		// final counters for Messages clients and the typed Responses bridge.
		raw = raw[:len(raw)-2]
		raw = append(raw, `,"input_tokens":`...)
		raw = strconv.AppendInt(raw, int64(h.inputTokens), 10)
		raw = append(raw, `,"cache_read_input_tokens":`...)
		raw = strconv.AppendInt(raw, int64(h.cachedInputTokens), 10)
		raw = append(raw, `,"cache_creation_input_tokens":`...)
		raw = strconv.AppendInt(raw, int64(h.cacheWriteTokens), 10)
		raw = append(raw, '}', '}')
	}
	h.writeEncodedSSELocked("message_delta", raw, err, true)
}

// writeMessageStartLocked emits the opening frame. It takes the lock as held.
func (h *streamHandler) writeMessageStartLocked(model string, inputTokens, outputTokens int) {
	if !h.isStream || h.hasReturn {
		return
	}
	raw, err := appendSSEMessageStart(nil, h.msgID, model, inputTokens, outputTokens)
	if err == nil && h.exclusiveUsage.active {
		raw = raw[:bytes.LastIndex(raw, []byte(`"usage":`))+len(`"usage":`)]
		raw, err = h.appendExclusiveWireUsage(raw, h.canonicalWireUsage || h.responseFormat == adapter.FormatOpenAI)
		raw = append(raw, '}', '}')
	}
	if err != nil {
		h.markWriteErrorLocked("message_start", err)
		return
	}
	h.messageStartWritten = true
	h.writeSSEBytesLocked("message_start", raw)
}

// ensureMessageStartLocked opens the stream if it has not been opened yet.
//
// The opening frame used to be written before the upstream was even called, so
// every streaming request committed 200 and a role chunk before it knew whether
// the upstream would produce anything. A refusal that arrived afterwards could
// then only be reported in band, which clients report as a truncated stream
// rather than a retryable status. Deferring it to the first real event keeps the
// response uncommitted while that is still true.
//
// It reports whether the stream is open and the caller may write its event. An
// error that already claimed the response is not reopened.
func (h *streamHandler) ensureMessageStartLocked() bool {
	if h.messageStartWritten {
		return true
	}
	if h.requestFailed || h.hasReturn || !h.isStream {
		return false
	}
	h.writeMessageStartLocked(h.pendingModel, h.inputTokens, 0)
	return h.messageStartWritten
}
