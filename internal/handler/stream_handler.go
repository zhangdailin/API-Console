package handler

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/logutil"
	"orchids-api/internal/perf"
	"orchids-api/internal/tiktoken"
	"orchids-api/internal/upstream"
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

// Event Handlers

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
