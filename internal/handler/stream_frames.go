package handler

import (
	"bytes"
	"io"
	"log/slog"
	"maps"
	"slices"

	"orchids-api/internal/adapter"
	"orchids-api/internal/logutil"
)

// sseFramePrefix returns the complete header bytes for a known wire event, or nil
// for anything the relay does not emit on the Anthropic event stream.
func sseFramePrefix(event string) []byte {
	switch event {
	case "message_start":
		return ssePrefixMessageStart
	case "message_delta":
		return ssePrefixMessageDelta
	case "message_stop":
		return ssePrefixMessageStop
	case "content_block_start":
		return ssePrefixContentBlockStart
	case "content_block_delta":
		return ssePrefixContentBlockDelta
	case "content_block_stop":
		return ssePrefixContentBlockStop
	}
	return nil
}

func mapKeys(m map[string]interface{}) []string {
	if m == nil {
		return nil
	}
	return slices.Collect(maps.Keys(m))
}

func writeOpenAIFrame(w io.Writer, payload []byte) error {
	if _, err := w.Write(sseDataPrefixBytes); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write(sseLineBreakBytes)
	return err
}

// writeSSEEventName writes the event name alone, for the rare frame whose event
// is not one of the relay's own framed types.
func writeSSEEventName(w io.Writer, event string) error {
	if sw, ok := w.(io.StringWriter); ok {
		_, err := sw.WriteString(event)
		return err
	}
	_, err := w.Write([]byte(event))
	return err
}

func writeSSEFrameBytes(w io.Writer, event string, data []byte) error {
	if prefix := sseFramePrefix(event); prefix != nil {
		if _, err := w.Write(prefix); err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		_, err := w.Write(sseLineBreakBytes)
		return err
	}
	// An event the relay does not frame itself (error, a vendor extension): spell
	// the header out field by field.
	if _, err := w.Write(sseEventPrefixBytes); err != nil {
		return err
	}
	if err := writeSSEEventName(w, event); err != nil {
		return err
	}
	if _, err := w.Write(sseDataJoinBytes); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err := w.Write(sseLineBreakBytes)
	return err
}

func (h *streamHandler) flushSSEWithLenLocked(event string, dataLen int, immediate bool, force bool) {
	if h.flusher == nil {
		return
	}
	// Do not wait for a batch before delivering the first reasoning or tool
	// delta. Low-cadence streams may take seconds to produce the next frame.
	if event == "content_block_delta" && !h.firstContentDeltaFlushed {
		h.firstContentDeltaFlushed = true
		immediate = true
	}
	if force || immediate {
		h.deferredFlushFrames = 0
		h.deferredFlushBytes = 0
		h.flusher.Flush()
		return
	}
	h.deferredFlushFrames++
	h.deferredFlushBytes += len(event) + dataLen + len(sseEventPrefix) + len(sseDataJoin) + len(sseLineBreak)
	if h.deferredFlushFrames >= sseDeferredFlushFrameThreshold || h.deferredFlushBytes >= sseDeferredFlushByteThreshold {
		h.deferredFlushFrames = 0
		h.deferredFlushBytes = 0
		h.flusher.Flush()
	}
}

// shouldFlushSSEImmediately reports whether a frame must reach the client at
// once. Text deltas are the streaming payload a user is waiting on; every other
// frame is either a boundary the client needs immediately or small enough to
// ride out in the next batch.
func shouldFlushSSEImmediately(event string, data []byte) bool {
	switch event {
	case "message_start", "message_delta", "message_stop", "content_block_start", "content_block_stop":
		return true
	case "content_block_delta":
		return bytes.Contains(data, sseTextDeltaMarker)
	}
	return true
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
	frame := h.sseFrameScratch[:0]
	if event != "" {
		if prefix := sseFramePrefix(event); prefix != nil {
			frame = append(frame, prefix...)
		} else {
			frame = append(frame, sseEventPrefixBytes...)
			frame = append(frame, event...)
			frame = append(frame, sseDataJoinBytes...)
		}
	} else {
		frame = append(frame, sseDataPrefixBytes...)
	}
	frame = append(frame, data...)
	frame = append(frame, sseLineBreakBytes...)
	h.sseFrameScratch = frame[:0]
	n, err := h.w.Write(frame)
	if err == nil && n != len(frame) {
		err = io.ErrShortWrite
	}
	return err
}

// emitSSEFrameLocked writes one frame the caller has already serialized, in the
// wire shape the response format asks for, and flushes it.
//
// It is the single place that knows the three differences between the writers
// the relay used to spell out per event: the OpenAI chunk adapter instead of an
// SSE frame, whether a completed response must flush immediately, and whether a
// per-frame diagnostics line follows.
func (h *streamHandler) emitSSEFrameLocked(event string, data []byte, immediate, final bool) {
	if !h.isStream {
		return
	}
	if h.responseFormat == adapter.FormatOpenAI {
		written, err := h.writeOpenAISSEBytes(event, data)
		if err != nil {
			h.markWriteErrorLocked(event, err)
			return
		}
		if written {
			h.flushSSEWithLenLocked(event, len(data), immediate, final)
		}
		if final && event == "message_stop" {
			if _, err := h.w.Write(sseDoneLineBytes); err != nil {
				h.markWriteErrorLocked(event, err)
				return
			}
			h.flushSSEWithLenLocked(event, len(sseDoneLine), true, true)
		}
		return
	}
	if err := h.writeStreamFrameLocked(event, data); err != nil {
		h.markWriteErrorLocked(event, err)
		return
	}
	h.flushSSEWithLenLocked(event, len(data), immediate, final)
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
	h.writeFinalSSEBytesLocked(event, data)
}

func (h *streamHandler) writeFinalSSEBytesLocked(event string, data []byte) {
	h.writeFinalSSEBytesLockedWithHint(event, data, false)
}

func (h *streamHandler) writeFinalSSEBytesLockedWithHint(event string, data []byte, immediate bool) {
	h.emitSSEFrameLocked(event, data, immediate, true)
}

func (h *streamHandler) writeSSEBytesLockedWithHint(event string, data []byte, immediate bool) {
	if !h.isStream || h.hasReturn {
		return
	}
	// Every event except the opening frame itself needs the stream open first, so
	// the client never sees a content frame before message_start.
	if event != "message_start" && !h.ensureMessageStartLocked() {
		return
	}
	h.emitSSEFrameLocked(event, data, immediate, false)
	if logutil.VerboseDiagnosticsEnabled() {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
}

func (h *streamHandler) writeSSEContentBlockStartToolUseLocked(index int, id, name string, final bool) {
	raw, err := appendSSEContentBlockStartToolUse(h.ssePayloadScratch[:0], index, id, name)
	if err != nil {
		h.markWriteErrorLocked("content_block_start", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_start", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_start", raw, true)
}

func (h *streamHandler) writeSSEContentBlockStartTextLocked(index int, final bool) {
	raw, err := appendSSEContentBlockStartText(h.ssePayloadScratch[:0], index)
	if err != nil {
		h.markWriteErrorLocked("content_block_start", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_start", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_start", raw, true)
}

func (h *streamHandler) writeSSEContentBlockDeltaInputJSONLocked(index int, partialJSON string, final bool) {
	raw, err := appendSSEContentBlockDeltaInputJSON(h.ssePayloadScratch[:0], index, partialJSON)
	if err != nil {
		h.markWriteErrorLocked("content_block_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_delta", raw, false)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_delta", raw, false)
}

func (h *streamHandler) writeSSEContentBlockDeltaTextLocked(index int, text string, final bool) {
	raw, err := appendSSEContentBlockDeltaText(h.ssePayloadScratch[:0], index, text)
	if err != nil {
		h.markWriteErrorLocked("content_block_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_delta", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_delta", raw, true)
}

func (h *streamHandler) writeSSEContentBlockDeltaThinkingLocked(index int, thinking string, final bool) {
	raw, err := appendSSEContentBlockDeltaThinking(h.ssePayloadScratch[:0], index, thinking)
	if err != nil {
		h.markWriteErrorLocked("content_block_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_delta", raw, false)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_delta", raw, false)
}

func (h *streamHandler) writeSSEContentBlockStopLocked(index int, final bool) {
	raw, err := appendSSEContentBlockStop(h.ssePayloadScratch[:0], index)
	if err != nil {
		h.markWriteErrorLocked("content_block_stop", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("content_block_stop", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("content_block_stop", raw, true)
}

func (h *streamHandler) writeSSEMessageDeltaLocked(stopReason string, outputTokens int, final bool) {
	raw, err := appendSSEMessageDelta(h.ssePayloadScratch[:0], stopReason, outputTokens)
	if err != nil {
		h.markWriteErrorLocked("message_delta", err)
		return
	}
	h.ssePayloadScratch = raw[:0]
	if final {
		h.writeFinalSSEBytesLockedWithHint("message_delta", raw, true)
		return
	}
	h.writeSSEBytesLockedWithHint("message_delta", raw, true)
}

// writeMessageStartLocked emits the opening frame. It takes the lock as held.
func (h *streamHandler) writeMessageStartLocked(model string, inputTokens, outputTokens int) {
	if !h.isStream || h.hasReturn {
		return
	}
	raw, err := appendSSEMessageStart(nil, h.msgID, model, inputTokens, outputTokens)
	if err != nil {
		h.markWriteErrorLocked("message_start", err)
		return
	}
	h.messageStartWritten = true
	h.writeSSEBytesLockedWithHint("message_start", raw, true)
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

// writeSSEBytesLocked writes one frame the caller has already serialized and
// lets the shared writer decide whether it must reach the client at once.
func (h *streamHandler) writeSSEBytesLocked(event string, data []byte) {
	if !h.isStream || h.hasReturn {
		return
	}
	h.emitSSEFrameLocked(event, data, shouldFlushSSEImmediately(event, data), false)
	if logutil.VerboseDiagnosticsEnabled() {
		slog.Debug("SSE Out", "event", event, "data_len", len(data))
	}
}
