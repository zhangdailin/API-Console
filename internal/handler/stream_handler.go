package handler

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"encoding/json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/logutil"
	"orchids-api/internal/middleware"
	"orchids-api/internal/perf"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

const (
	sseEventPrefix                 = "event: "
	sseDataPrefix                  = "data: "
	sseLineBreak                   = "\n\n"
	sseDataJoin                    = "\ndata: "
	sseDoneLine                    = "data: [DONE]\n\n"
	sseKeepAlive                   = ": keep-alive\n\n"
	sseDeferredFlushFrameThreshold = 4
	sseDeferredFlushByteThreshold  = 2048
)

var (
	sseTextDeltaMarker  = []byte(`"type":"text_delta"`)
	sseDoneLineBytes    = []byte(sseDoneLine)
	sseKeepAliveBytes   = []byte(sseKeepAlive)
	sseEventPrefixBytes = []byte(sseEventPrefix)
	sseDataPrefixBytes  = []byte(sseDataPrefix)
	sseLineBreakBytes   = []byte(sseLineBreak)
	sseDataJoinBytes    = []byte(sseDataJoin)
	// Complete "event: <name>\ndata: " frame headers, one per event the relay
	// emits. Folding the prefix, the name and the data join into a single slice
	// makes a frame header one Write instead of three, and looking them up
	// through the switch below compiles to length-and-bytes comparisons against
	// the constants the callers pass — replacing a map hash on every frame.
	ssePrefixMessageStart      = []byte(sseEventPrefix + "message_start" + sseDataJoin)
	ssePrefixMessageDelta      = []byte(sseEventPrefix + "message_delta" + sseDataJoin)
	ssePrefixMessageStop       = []byte(sseEventPrefix + "message_stop" + sseDataJoin)
	ssePrefixContentBlockStart = []byte(sseEventPrefix + "content_block_start" + sseDataJoin)
	ssePrefixContentBlockDelta = []byte(sseEventPrefix + "content_block_delta" + sseDataJoin)
	ssePrefixContentBlockStop  = []byte(sseEventPrefix + "content_block_stop" + sseDataJoin)
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

// --- json helper functions removed ---

// --- sse framing functions removed ---

type streamHandler struct {
	cancelUpstream context.CancelFunc
	// Configuration
	config              *config.Config
	isStream            bool
	suppressThinking    bool
	useUpstreamUsage    bool
	responseFormat      adapter.ResponseFormat
	disallowToolCalls   bool
	surfaceToolRejects  bool
	allowedToolNames    map[string]struct{}
	clientTools         []interface{}
	emptyOutputFallback string
	// structured validates the finished answer of a strict structured-output
	// request. It is nil unless the caller sent `strict: true` with a schema
	// this gateway could read, so an ordinary request pays nothing here.
	structured structuredOutputCheck

	// HTTP Response
	w       http.ResponseWriter
	flusher http.Flusher

	// State
	mu sync.Mutex
	// returned mirrors hasReturn for the lock-free early-out at the top of
	// handleMessage. The check runs once per upstream event, and taking h.mu just
	// to read one bool was measurable on the per-token path.
	returned         atomic.Bool
	blockIndex       int
	msgID            string
	startTime        time.Time
	hasReturn        bool
	requestFailed    bool // terminal error body already owns non-stream response
	completionLogged bool
	// messageStartWritten records that the opening frame, and therefore the HTTP
	// status, has been committed to the client. Until it is set nothing has been
	// sent, so a failed attempt can still be answered with a real HTTP status
	// instead of an in-band error on a stream the client has already accepted.
	messageStartWritten bool
	// pendingModel is the model the deferred opening frame reports.
	pendingModel             string
	hasReasoningOutput       bool
	finalStopReason          string
	outputTokens             int
	inputTokens              int
	cachedInputTokens        int
	cacheWriteTokens         int
	reasoningTokens          int
	usageMetadata            map[string]interface{}
	activeThinkingBlockIndex int
	activeThinkingSSEIndex   int
	activeTextBlockIndex     int
	activeTextSSEIndex       int
	activeBlockType          string // "thinking", "text", "tool_use"

	// Buffers and Builders
	responseText    *strings.Builder
	outputEstimator tiktoken.Estimator
	// History builders are indexed by content-block position, not keyed by it.
	// The position is exactly len(contentBlocks)-1 at creation, so the indices are
	// dense and monotonic and a slice replaces the map the per-token delta paths
	// used to hash into once per frame.
	textBlockBuilders        []*strings.Builder
	thinkingBlockBuilders    []*strings.Builder
	thinkingBlockSigs        []string
	contentBlocks            []map[string]interface{}
	pendingThinkingSig       string
	hasTextOutput            bool
	deferredFlushFrames      int
	deferredFlushBytes       int
	firstContentDeltaFlushed bool
	openAIChunkScratch       []byte
	sseFrameScratch          []byte
	ssePayloadScratch        []byte

	// Tool Handling (proxy mode only)
	pendingToolCalls    []toolCall
	toolCallHandled     map[string]bool
	toolCallEmitted     map[string]struct{}
	toolCallCount       int
	suppressedToolCalls int

	// Logger
	logger *debug.Logger
}

func responseMessageID(format adapter.ResponseFormat) string {
	if format == adapter.FormatOpenAI {
		return "chatcmpl-" + randomSessionID()
	}
	return "msg_" + randomSessionID()
}

func newStreamHandler(
	cfg *config.Config,
	w http.ResponseWriter,
	logger *debug.Logger,
	suppressThinking bool,
	isStream bool,
	responseFormat adapter.ResponseFormat,
) *streamHandler {
	var flusher http.Flusher
	if isStream {
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
	}

	h := &streamHandler{
		config:           cfg,
		w:                w,
		flusher:          flusher,
		isStream:         isStream,
		logger:           logger,
		suppressThinking: suppressThinking,
		responseFormat:   responseFormat,

		blockIndex:               -1,
		responseText:             perf.AcquireStringBuilder(),
		toolCallHandled:          make(map[string]bool),
		toolCallEmitted:          make(map[string]struct{}),
		allowedToolNames:         make(map[string]struct{}),
		msgID:                    responseMessageID(responseFormat),
		startTime:                time.Now(),
		activeThinkingBlockIndex: -1,
		activeThinkingSSEIndex:   -1,
		activeTextBlockIndex:     -1,
		activeTextSSEIndex:       -1,
		activeBlockType:          "",
		openAIChunkScratch:       make([]byte, 0, 512),
		ssePayloadScratch:        make([]byte, 0, 512),
	}
	return h
}

func (h *streamHandler) setDisallowToolCalls(disallow bool) {
	h.mu.Lock()
	h.disallowToolCalls = disallow
	h.mu.Unlock()
}

func (h *streamHandler) setAllowedToolNames(names []string) {
	h.mu.Lock()
	clear(h.allowedToolNames)
	for _, name := range names {
		key := strings.TrimSpace(name)
		if key == "" {
			continue
		}
		h.allowedToolNames[key] = struct{}{}
	}
	h.mu.Unlock()
}

func (h *streamHandler) setSurfaceToolRejects(surface bool) {
	h.mu.Lock()
	h.surfaceToolRejects = surface
	h.mu.Unlock()
}

func (h *streamHandler) setClientTools(tools []interface{}) {
	h.mu.Lock()
	h.clientTools = tools
	h.mu.Unlock()
}

func (h *streamHandler) setEmptyOutputFallback(text string) {
	h.mu.Lock()
	h.emptyOutputFallback = strings.TrimSpace(text)
	h.mu.Unlock()
}

// setStructuredOutput installs the validator a strict structured-output request
// carries. Nil is the ordinary case: a request that asked for no schema, or a
// non-strict one, has nothing to enforce.
func (h *streamHandler) setStructuredOutput(check structuredOutputCheck) {
	h.mu.Lock()
	h.structured = check
	h.mu.Unlock()
}

// structuredOutputCheck validates one completed assistant answer against the
// schema the caller declared. It is a function rather than an interface so the
// handler keeps no reference to the request's schema object.
type structuredOutputCheck func(text string) error

// validateStructuredAnswer checks the answer accumulated so far against the
// schema of a strict structured-output request. It returns nil when the request
// declared no schema, and when the answer matches.
//
// Only the visible text is checked: reasoning is not part of the answer the
// caller parses, and tool calls are validated by the tool pipeline already. An
// empty answer is left to the empty-output fallback below rather than rejected
// here, so a request that produced nothing still reports that.
func (h *streamHandler) validateStructuredAnswer() error {
	h.mu.Lock()
	check := h.structured
	text := h.responseText.String()
	h.mu.Unlock()
	if check == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	return check(text)
}

// growBuilderSlots extends a builder slice so idx is addressable. Content-block
// indices are dense and monotonic, so this grows by one slot per block in
// practice and keeps append's amortised doubling for the rest.
func growBuilderSlots(slots []*strings.Builder, idx int) []*strings.Builder {
	for len(slots) <= idx {
		slots = append(slots, nil)
	}
	return slots
}

// growSigSlots extends the signature slice so idx is addressable. The slice is
// grown only by the thinking-block creation path, so its length is exactly one
// past the highest index that path ever wrote — which is what makes the bounds
// check in thinkingBlockSigAt reproduce the map's "key present" test.
func growSigSlots(sigs []string, idx int) []string {
	for len(sigs) <= idx {
		sigs = append(sigs, "")
	}
	return sigs
}

// thinkingBlockSigAt returns the stored signature for a thinking-block index. The
// boolean reproduces the map's "key present" result, so an index past the end
// reads as absent rather than as present-and-empty — the difference that keeps
// the callers below from writing outside the slice.
func (h *streamHandler) thinkingBlockSigAt(idx int) (string, bool) {
	if idx < 0 || idx >= len(h.thinkingBlockSigs) {
		return "", false
	}
	return h.thinkingBlockSigs[idx], true
}

// builderAt returns the history builder already recorded at idx, or nil. It
// never allocates, for readers that must not grow the slice.
func builderAt(slots []*strings.Builder, idx int) *strings.Builder {
	if idx < 0 || idx >= len(slots) {
		return nil
	}
	return slots[idx]
}

// historyBuilderAt returns the history builder for a content-block index,
// allocating the slot and the builder on first use. The caller must hold h.mu.
func (h *streamHandler) historyBuilderAt(slots *[]*strings.Builder, idx int) *strings.Builder {
	if idx < 0 {
		return nil
	}
	*slots = growBuilderSlots(*slots, idx)
	if (*slots)[idx] == nil {
		(*slots)[idx] = perf.AcquireStringBuilder()
	}
	return (*slots)[idx]
}

func (h *streamHandler) release() {
	perf.ReleaseStringBuilder(h.responseText)
	for _, sb := range h.textBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	for _, sb := range h.thinkingBlockBuilders {
		perf.ReleaseStringBuilder(sb)
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

// hasCommitted reports whether any byte of the response has been sent to the
// client. Before that, a failure can still carry a real HTTP status.
func (h *streamHandler) hasCommitted() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.messageStartWritten || h.hasReturn
}

func (h *streamHandler) writeKeepAlive() {
	if !h.isStream {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hasReturn {
		return
	}
	// A keep-alive comment must not commit an empty 200 response. The client
	// can wait for the shared queue without a body; if the retry budget expires,
	// reportRequestFailure can still return an actual HTTP 429. Opening an
	// Anthropic message_start here made queue refusals look like interrupted
	// streams (HTTP 200 plus an in-band error) after 15 silent seconds.
	if !h.messageStartWritten {
		return
	}
	if _, err := h.w.Write(sseKeepAliveBytes); err != nil {
		h.markWriteErrorLocked("keep-alive", err)
		return
	}
	h.flushSSEWithLenLocked("keep-alive", len(sseKeepAlive), true, true)
}

// addOutputTokens folds a fragment into the running output estimate.
//
// Token accounting lives under h.mu rather than a mutex of its own: resetRoundState
// already cleared outputTokens and the estimator under h.mu, so a second mutex
// gave no real exclusion over the same fields. One lock for the handler's state
// is what lets the per-token delta paths take the lock once per frame.
func (h *streamHandler) addOutputTokens(text string) {
	if text == "" {
		return
	}
	h.mu.Lock()
	if !h.useUpstreamUsage {
		h.outputEstimator.Add(text)
	}
	h.mu.Unlock()
}

func (h *streamHandler) finalizeOutputTokens() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.useUpstreamUsage {
		return
	}
	h.outputTokens = h.outputEstimator.Count()
}

// applyUpstreamUsageTokens folds one provider usage payload into the reported
// totals under one lock. Omitted fields keep their local estimates; metadata
// alone still counts as provider usage evidence.
func (h *streamHandler) applyUpstreamUsageTokens(usage map[string]interface{}) {
	if len(usage) == 0 {
		return
	}
	read := func(keys ...string) (int, bool) {
		for _, key := range keys {
			if value, ok := getUsageIntValue(usage, key); ok {
				return value, true
			}
		}
		return 0, false
	}
	input, hasInput := read("inputTokens", "input_tokens")
	output, hasOutput := read("outputTokens", "output_tokens")
	cached, hasCached := read("cacheReadTokens", "cache_read_tokens")
	cacheWrite, hasCacheWrite := read("cacheWriteTokens", "cache_creation_input_tokens")
	reasoning, hasReasoning := read("reasoningTokens", "reasoning_tokens")
	_, hasCredits := usage["credits"]
	_, hasOriginalCredits := usage["original_credits"]
	metadataKeys := []string{"billable", "cacheable_tokens", "firstTokenDuration", "totalDuration", "serverDuration"}
	hasMetadata := false
	for _, key := range metadataKeys {
		if _, ok := usage[key]; ok {
			hasMetadata = true
		}
	}
	if !hasInput && !hasOutput && !hasCached && !hasCacheWrite && !hasReasoning && !hasCredits && !hasOriginalCredits && !hasMetadata {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if hasInput || hasOutput || hasCached || hasCacheWrite || hasReasoning || hasCredits || hasOriginalCredits {
		h.useUpstreamUsage = true
	}
	if hasInput {
		h.inputTokens = input
	}
	if hasOutput {
		h.outputTokens = output
	}
	if hasCached {
		h.cachedInputTokens = cached
	}
	if hasCacheWrite {
		h.cacheWriteTokens = cacheWrite
	}
	if hasReasoning {
		h.reasoningTokens = reasoning
	}
	if hasCredits || hasOriginalCredits || hasMetadata {
		if h.usageMetadata == nil {
			h.usageMetadata = make(map[string]interface{}, 7)
		}
		for _, key := range metadataKeys {
			if value, ok := usage[key]; ok {
				h.usageMetadata[key] = value
			}
		}
		if hasCredits {
			h.usageMetadata["credits"] = usage["credits"]
		}
		if hasOriginalCredits {
			h.usageMetadata["original_credits"] = usage["original_credits"]
		}
	}
}

func getUsageIntValue(usage map[string]interface{}, key string) (int, bool) {
	value, ok := usage[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		n, err := typed.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func (h *streamHandler) setUsageTokens(input, output int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.useUpstreamUsage = true
	if input >= 0 {
		h.inputTokens = input
	}
	if output >= 0 {
		h.outputTokens = output
	}
}

func (h *streamHandler) resetRoundState() {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Ensure any currently open block is closed before resetting state
	h.closeActiveBlockLocked()

	// The block indices below are the SSE positions handed to the client, so they
	// are reset together with the per-block bookkeeping: a round that starts with
	// a stale blockIndex would emit a content_block_delta for an index the client
	// never saw a start for, which the client answers with "Mismatched content
	// block type". h.blockIndex counts every block the round has opened.
	h.activeThinkingBlockIndex = -1
	h.activeThinkingSSEIndex = -1
	h.activeTextBlockIndex = -1
	h.activeTextSSEIndex = -1
	h.activeBlockType = ""
	h.hasReturn = false
	h.returned.Store(false)

	h.responseText.Reset()
	h.contentBlocks = nil

	for _, sb := range h.textBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	h.textBlockBuilders = h.textBlockBuilders[:0]

	for _, sb := range h.thinkingBlockBuilders {
		perf.ReleaseStringBuilder(sb)
	}
	h.thinkingBlockBuilders = h.thinkingBlockBuilders[:0]
	h.thinkingBlockSigs = h.thinkingBlockSigs[:0]

	h.pendingToolCalls = nil
	clear(h.toolCallHandled)
	clear(h.toolCallEmitted)
	h.toolCallCount = 0
	h.outputTokens = 0
	h.cachedInputTokens = 0
	h.cacheWriteTokens = 0
	h.reasoningTokens = 0
	h.usageMetadata = nil
	h.completionLogged = false
	h.outputEstimator.Reset()
	h.useUpstreamUsage = false
	h.finalStopReason = ""
	h.hasTextOutput = false
	h.deferredFlushFrames = 0
	h.deferredFlushBytes = 0
	h.firstContentDeltaFlushed = false
}

func (h *streamHandler) emitToolCallNonStream(call toolCall) {
	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	inputJSON := strings.TrimSpace(call.input)
	inputJSON = util.FirstNonEmptyUntrimmed(inputJSON, "{}")
	var inputValue interface{}
	if err := json.Unmarshal([]byte(inputJSON), &inputValue); err != nil {
		inputValue = map[string]interface{}{}
	}
	h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
		"type":  "tool_use",
		"id":    call.id,
		"name":  call.name,
		"input": inputValue,
	})
}

func (h *streamHandler) emitToolCallStream(call toolCall, idx int, final bool) {
	if call.id == "" {
		return
	}

	h.addOutputTokens(call.name)
	h.addOutputTokens(call.input)
	inputJSON := strings.TrimSpace(call.input)
	inputJSON = util.FirstNonEmptyUntrimmed(inputJSON, "{}")

	h.mu.Lock()
	defer h.mu.Unlock()
	if idx < 0 {
		h.blockIndex++
		idx = h.blockIndex
	}
	h.writeSSEContentBlockStartToolUseLocked(idx, call.id, call.name, final)
	h.writeSSEContentBlockDeltaInputJSONLocked(idx, inputJSON, final)
	h.writeSSEContentBlockStopLocked(idx, final)
}

// emitToolUseFromInput emits a single tool_use block once the full input is available.
func (h *streamHandler) emitToolUseFromInput(toolID, toolName, inputStr string) {
	if toolID == "" || toolName == "" {
		return
	}
	if _, ok := h.toolCallEmitted[toolID]; ok {
		return
	}
	h.toolCallEmitted[toolID] = struct{}{}

	h.addOutputTokens(toolName)
	inputJSON := strings.TrimSpace(inputStr)
	inputJSON = util.FirstNonEmptyUntrimmed(inputJSON, "{}")

	h.mu.Lock()
	h.toolCallCount++
	h.blockIndex++
	idx := h.blockIndex
	h.writeSSEContentBlockStartToolUseLocked(idx, toolID, toolName, false)
	h.writeSSEContentBlockDeltaInputJSONLocked(idx, inputJSON, false)
	h.writeSSEContentBlockStopLocked(idx, false)
	h.mu.Unlock()
}

func (h *streamHandler) flushPendingToolCalls() {
	h.mu.Lock()
	calls := slices.Clone(h.pendingToolCalls)
	h.pendingToolCalls = nil
	h.mu.Unlock()

	for _, call := range calls {
		if h.isStream {
			h.emitToolCallStream(call, -1, true)
		} else {
			h.emitToolCallNonStream(call)
		}
	}
}

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

func (h *streamHandler) ensureBlock(blockType string) int {
	if blockType == "thinking" && h.suppressThinking {
		return -1
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// If already in a block of a different type, close it
	if h.activeBlockType != "" && h.activeBlockType != blockType {
		h.closeActiveBlockLocked()
	}

	// If already in the correct block type, return current index
	if h.activeBlockType == blockType {
		if blockType == "thinking" {
			return h.activeThinkingSSEIndex
		}
		if blockType == "text" {
			return h.activeTextSSEIndex
		}
	}

	// Start new block
	h.blockIndex++
	sseIdx := h.blockIndex
	h.activeBlockType = blockType

	switch blockType {
	case "thinking":
		signature := h.pendingThinkingSig
		h.pendingThinkingSig = ""
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type":      "thinking",
			"signature": signature,
		})
		internalIdx := len(h.contentBlocks) - 1
		h.activeThinkingBlockIndex = internalIdx
		h.activeThinkingSSEIndex = sseIdx
		h.thinkingBlockBuilders = growBuilderSlots(h.thinkingBlockBuilders, internalIdx)
		h.thinkingBlockBuilders[internalIdx] = perf.AcquireStringBuilder()
		h.thinkingBlockSigs = growSigSlots(h.thinkingBlockSigs, internalIdx)
		h.thinkingBlockSigs[internalIdx] = signature

		raw, err := appendSSEContentBlockStartThinking(h.ssePayloadScratch[:0], sseIdx, signature)
		if err != nil {
			h.markWriteErrorLocked("content_block_start", err)
			break
		}
		h.ssePayloadScratch = raw[:0]
		h.writeSSEBytesLockedWithHint("content_block_start", raw, true)
	case "text":
		h.contentBlocks = append(h.contentBlocks, map[string]interface{}{
			"type": "text",
		})
		internalIdx := len(h.contentBlocks) - 1
		h.activeTextBlockIndex = internalIdx
		h.activeTextSSEIndex = sseIdx
		h.textBlockBuilders = growBuilderSlots(h.textBlockBuilders, internalIdx)
		h.textBlockBuilders[internalIdx] = perf.AcquireStringBuilder()

		h.writeSSEContentBlockStartTextLocked(sseIdx, false)
	}

	return sseIdx
}

func (h *streamHandler) closeActiveBlock() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeActiveBlockLocked()
}

func (h *streamHandler) popActiveBlockStopDataLocked() ([]byte, bool) {
	if h.activeBlockType == "" {
		return nil, false
	}

	var sseIdx int
	switch h.activeBlockType {
	case "thinking":
		sseIdx = h.activeThinkingSSEIndex
		h.activeThinkingBlockIndex = -1
		h.activeThinkingSSEIndex = -1
	case "text":
		sseIdx = h.activeTextSSEIndex
		h.activeTextBlockIndex = -1
		h.activeTextSSEIndex = -1
	default:
		// tool_use and others are usually handled as single-event blocks or managed separately
		h.activeBlockType = ""
		return nil, false
	}

	h.activeBlockType = ""

	stopData, err := marshalSSEContentBlockStopBytes(sseIdx)
	if err != nil {
		slog.Error("Failed to marshal content_block_stop", "error", err)
	}
	if err != nil {
		return nil, false
	}
	return stopData, true
}

func (h *streamHandler) closeActiveBlockLocked() {
	stopData, ok := h.popActiveBlockStopDataLocked()
	if !ok {
		return
	}
	h.writeSSEBytesLocked("content_block_stop", stopData)
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

// Event Handlers

func (h *streamHandler) handleToolCallAfterChecks(call toolCall) {
	h.mu.Lock()
	h.pendingToolCalls = append(h.pendingToolCalls, call)
	h.toolCallCount++
	h.mu.Unlock()
}

func (h *streamHandler) shouldAcceptToolCall(call toolCall) bool {
	h.mu.Lock()
	disallowToolCalls := h.disallowToolCalls
	if disallowToolCalls && h.surfaceToolRejects && h.emptyOutputFallback == "" {
		h.emptyOutputFallback = suppressedWriteContentFallback(call)
	}
	allowedTool := true
	if len(h.allowedToolNames) > 0 {
		name := strings.TrimSpace(call.name)
		_, allowedTool = h.allowedToolNames[name]
	}
	if !allowedTool && h.surfaceToolRejects && h.emptyOutputFallback == "" {
		h.emptyOutputFallback = "The upstream model attempted to use a tool that is not available in this request."
	}
	if disallowToolCalls && h.surfaceToolRejects && h.emptyOutputFallback == "" {
		h.emptyOutputFallback = "This request did not provide a compatible tool for the attempted operation."
	}
	if disallowToolCalls {
		h.suppressedToolCalls++
	}
	if !allowedTool {
		h.suppressedToolCalls++
	}
	h.mu.Unlock()
	if disallowToolCalls {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("tool call suppressed by no-tools gate", "tool", call.name, "input", call.input)
		}
		return false
	}
	if !allowedTool {
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("tool call suppressed because it is not declared in the current request", "tool", call.name, "input", call.input)
		}
		return false
	}

	if !validToolCallInput(call.name, call.input) {
		h.mu.Lock()
		h.suppressedToolCalls++
		if h.surfaceToolRejects && h.emptyOutputFallback == "" {
			h.emptyOutputFallback = "The upstream model returned an invalid tool request."
		}
		h.mu.Unlock()
		if h.config != nil && h.config.DebugEnabled {
			slog.Debug("invalid tool call suppressed", "tool", call.name, "input", call.input)
		}
		return false
	}

	return true
}

func normalizeToolNameKey(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

func suppressedWriteContentFallback(call toolCall) string {
	if normalizeToolNameKey(call.name) != "write" {
		return ""
	}
	var input struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(call.input), &input); err != nil {
		return ""
	}
	return strings.TrimSpace(input.Content)
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

// extractThinkingSignature extracts a signature from event or event.data.
func extractThinkingSignature(event map[string]interface{}) string {
	if event == nil {
		return ""
	}
	if sig, ok := event["signature"].(string); ok {
		return strings.TrimSpace(sig)
	}
	if data, ok := event["data"].(map[string]interface{}); ok {
		if sig, ok := data["signature"].(string); ok {
			return strings.TrimSpace(sig)
		}
	}
	return ""
}

func (h *streamHandler) handleMessage(msg upstream.SSEMessage) {
	if logutil.VerboseDiagnosticsEnabled() && msg.Type != "content_block_delta" {
		fields := []any{"type", msg.Type}
		if msg.Event != nil {
			// Avoid leaking secrets in logs: only log high-level shape.
			evtType, _ := msg.Event["type"].(string)
			fields = append(fields, "event_type", evtType)
			if delta, ok := msg.Event["delta"]; ok {
				fields = append(fields, "has_delta", delta != nil)
			}
			if data, ok := msg.Event["data"].(map[string]interface{}); ok {
				fields = append(fields, "data_keys", mapKeys(data))
				if msgStr, ok := data["message"].(string); ok {
					fields = append(fields, "data_message_len", len(msgStr))
				}
			}
			fields = append(fields, "event_keys", mapKeys(msg.Event))
		}
		slog.Debug("Incoming SSE", fields...)
	}
	// Lock-free early-out: this runs once per upstream event, and taking h.mu only
	// to read a bool was measurable on the per-token path.
	if h.returned.Load() {
		return
	}

	eventKey := msg.Type
	if msg.Type == "model" && msg.Event != nil {
		if evtType, ok := msg.Event["type"].(string); ok {
			eventKey = "model." + evtType
		}
	}

	// Instrument: Log detailed error info. A trailing ".error" is subsumed by the
	// substring test, so spelling both only bought a second scan of the event key
	// on every frame that was not an error.
	if strings.Contains(eventKey, "error") {
		if msg.Event != nil {
			if data, ok := msg.Event["data"]; ok {
				slog.Warn("SSE Error Payload", "type", eventKey, "data", data)
			}
		}
	}
	if h.suppressThinking {
		if strings.HasPrefix(eventKey, "model.reasoning-") {
			if eventKey == "model.reasoning-delta" {
				if delta, _ := msg.Event["delta"].(string); delta != "" {
					h.mu.Lock()
					h.hasReasoningOutput = true
					h.mu.Unlock()
				}
			}
			return
		}
	}

	switch eventKey {
	case "model.actual_model":
		slog.Warn("Ignoring upstream model substitution event")

	case "model.reasoning-start":
		h.pendingThinkingSig = ""
		if sig := extractThinkingSignature(msg.Event); sig != "" {
			h.pendingThinkingSig = sig
			h.ensureBlock("thinking")
		}

	case "model.reasoning-delta":
		// Same single-critical-section shape as model.text-delta: the reasoning
		// stream is one delta per token too, and it previously took the lock six
		// times per frame. pendingThinkingSig is read under the lock because
		// ensureBlock consumes and clears it under the same lock.
		h.mu.Lock()
		sig := h.pendingThinkingSig
		h.mu.Unlock()
		if sig == "" {
			sig = extractThinkingSignature(msg.Event)
			if sig != "" {
				h.mu.Lock()
				h.pendingThinkingSig = sig
				h.mu.Unlock()
			}
		}
		delta, _ := msg.Event["delta"].(string)
		if delta == "" {
			if sig != "" {
				h.ensureBlock("thinking")
				h.mu.Lock()
				internalIdx := h.activeThinkingBlockIndex
				if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
					if existing, ok := h.thinkingBlockSigAt(internalIdx); ok && existing == "" {
						h.thinkingBlockSigs[internalIdx] = sig
						h.contentBlocks[internalIdx]["signature"] = sig
					}
				}
				h.mu.Unlock()
			}
			return
		}
		h.mu.Lock()
		h.hasReasoningOutput = true
		sseIdx := h.activeThinkingSSEIndex
		internalIdx := h.activeThinkingBlockIndex
		if sig != "" && internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
			if existing, ok := h.thinkingBlockSigAt(internalIdx); ok && existing == "" {
				h.thinkingBlockSigs[internalIdx] = sig
				h.contentBlocks[internalIdx]["signature"] = sig
			}
		}
		if sseIdx < 0 {
			// If we get a delta but no thinking block is active, open one.
			h.mu.Unlock()
			sseIdx = h.ensureBlock("thinking")
			h.mu.Lock()
			internalIdx = h.activeThinkingBlockIndex
		}
		if h.isStream && !h.useUpstreamUsage {
			h.outputTokens += tiktoken.EstimateTextTokens(delta)
		}
		// Always update internal state for history
		if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
			builder := h.historyBuilderAt(&h.thinkingBlockBuilders, internalIdx)
			builder.WriteString(delta)
		}
		h.writeSSEContentBlockDeltaThinkingLocked(sseIdx, delta, false)
		h.mu.Unlock()

	case "model.reasoning-end":
		h.closeActiveBlock()

	case "model.text-start":
		h.ensureBlock("text")

	case "model.text-delta":
		delta, _ := msg.Event["delta"].(string)
		if delta == "" {
			return
		}

		// One critical section for the whole frame. This case used to take the
		// lock five separate times per delta — once to mark text output, once for
		// the block indices, once for the estimator and twice for the history
		// builder and the SSE write — which at one delta per token made lock
		// traffic the single largest cost in the relay. The block-opening path
		// owns the lock itself, so it is the one step taken outside this section.
		h.mu.Lock()
		h.hasTextOutput = true
		sseIdx := h.activeTextSSEIndex
		internalIdx := h.activeTextBlockIndex
		if sseIdx < 0 {
			// If we get a delta but no text block is active, open one.
			h.mu.Unlock()
			sseIdx = h.ensureBlock("text")
			h.mu.Lock()
			internalIdx = h.activeTextBlockIndex
		}
		if !h.useUpstreamUsage {
			h.outputEstimator.Add(delta)
		}
		if !h.isStream {
			h.responseText.WriteString(delta)
		}
		// Always update internal state for history
		if internalIdx >= 0 && internalIdx < len(h.contentBlocks) {
			builder := h.historyBuilderAt(&h.textBlockBuilders, internalIdx)
			builder.WriteString(delta)
		}
		h.writeSSEContentBlockDeltaTextLocked(sseIdx, delta, false)
		h.mu.Unlock()

	case "model.text-end":
		h.closeActiveBlock()

	case "model.tool-call":
		toolID, _ := msg.Event["toolCallId"].(string)
		toolName, _ := msg.Event["toolName"].(string)
		inputStr, _ := msg.Event["input"].(string)
		if toolID == "" {
			return
		}
		if h.toolCallHandled[toolID] {
			return
		}
		call := toolCall{id: toolID, name: toolName, input: inputStr}
		if !h.shouldAcceptToolCall(call) {
			return
		}
		h.toolCallHandled[toolID] = true
		if h.isStream {
			h.emitToolUseFromInput(toolID, toolName, inputStr)
			return
		}
		h.handleToolCallAfterChecks(call)

	case "model.tokens-used":
		h.applyUpstreamUsageTokens(msg.Event)
		return

	case "model.finish":
		stopReason := "end_turn"
		if usage, ok := msg.Event["usage"].(map[string]interface{}); ok {
			h.applyUpstreamUsageTokens(usage)
		}
		if finishReason, ok := msg.Event["finishReason"].(string); ok {
			switch finishReason {
			case "tool-calls", "tool_use":
				stopReason = "tool_use"
			case "stop", "end_turn":
				stopReason = "end_turn"
			case "max_tokens", "max_token_limit":
				stopReason = "max_tokens"
			}
		}

		h.mu.Lock()
		toolUseEmitted := len(h.toolCallEmitted) > 0
		hadToolCalls := h.toolCallCount > 0 ||
			len(h.pendingToolCalls) > 0 ||
			toolUseEmitted
		h.mu.Unlock()

		// Force stopReason to tool_use if we have emitted tool calls
		if toolUseEmitted {
			stopReason = "tool_use"
		}

		// If upstream claims tool_use but we didn't actually handle any tool calls, treat as end_turn.
		if stopReason == "tool_use" && !hadToolCalls {
			stopReason = "end_turn"
		}

		h.closeActiveBlock()
		h.finishResponse(stopReason)
	}
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

// Native function arguments must be a JSON object.
func validToolCallInput(name, input string) bool {
	if strings.TrimSpace(name) == "" {
		return false
	}
	var object map[string]json.RawMessage
	return json.Unmarshal([]byte(input), &object) == nil && object != nil
}
