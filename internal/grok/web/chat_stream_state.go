package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/grokbase"
)

// webChatStream is the state of one streamed Chat Completions answer relayed
// from grok.com. grok.com sends both token deltas and growing message
// snapshots; the stream prefers the snapshots (they carry the final text) and
// falls back to the deltas when no snapshot arrives.
type webChatStream struct {
	h          *Handler
	w          http.ResponseWriter
	flusher    http.Flusher
	req        *ChatCompletionsRequest
	model      string
	spec       ModelSpec
	token      string
	publicBase string
	logger     *debug.Logger

	id, fingerprint string
	sentRole        bool
	sentAny         bool
	finalUsage      map[string]any
	chunkScratch    []byte

	downstreamWriteErr error

	// Media: full image variants are preferred over -part-0 previews.
	enableMediaExtraction bool
	seenFull              map[string]bool
	pendingPart           map[string]string
	emitted               map[string]bool
	textRefCollector      *streamTextImageRefCollector
	finalSnapshotMarkdown []string

	// Text: the latest snapshot, what of it was emitted, and the delta fallback.
	lastMessage          string
	sawModelMessage      bool
	tokenFallback        strings.Builder
	reasoningContent     strings.Builder
	lastThinkingMessage  string
	emittedModelMessage  string
	pendingModelMessage  string
	finalSnapshotEnabled bool
	markup               *streamMarkupFilter

	// Tools: calls are parsed out of the text and sent as tool_calls chunks.
	toolStreamMode bool
	toolParser     toolCallParser
	toolPump       *toolStreamPump
	toolCallIndex  int
}

func newWebChatStream(h *Handler, w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice any, logger *debug.Logger) *webChatStream {
	s := &webChatStream{
		h: h, w: w, flusher: streamResponseHeaders(w), req: req, model: model, spec: spec,
		token: token, publicBase: publicBase, logger: logger,
		id:                    "chatcmpl_" + randomHex(8),
		enableMediaExtraction: hasAttachments || spec.IsVideo,
		toolStreamMode:        toolCallsEnabled(tools, toolChoice),
		toolParser:            newToolCallParser(tools, toolChoice),
		finalSnapshotMarkdown: make([]string, 0, 4),
		chunkScratch:          make([]byte, 0, 256),
	}
	if s.enableMediaExtraction {
		s.seenFull = map[string]bool{}
		s.pendingPart = map[string]string{}
		s.emitted = map[string]bool{}
		s.textRefCollector = newStreamTextImageRefCollector()
	}
	s.finalSnapshotEnabled = !s.toolStreamMode && !spec.IsVideo
	if s.toolStreamMode {
		s.toolPump = newToolStreamPump(s.toolParser)
	}
	if !hasAttachments && !s.toolStreamMode {
		s.markup = &streamMarkupFilter{}
	}
	return s
}

// auditOutcome journals the usage of a stream that finished with one.
func (s *webChatStream) auditOutcome() {
	if len(s.finalUsage) > 0 {
		s.h.auditChatOutcome(context.Background(), s.req.account, s.req, chatOutcome{Finish: "stop", Usage: s.finalUsage, UsageSource: audit.UsageSourceEstimated})
	}
}

// --- emitters ---------------------------------------------------------------

func (s *webChatStream) emitRaw(raw []byte) {
	if s.downstreamWriteErr == nil {
		s.downstreamWriteErr = writeSSELog(s.w, s.flusher, s.logger, raw)
	}
}

func (s *webChatStream) emitChunk(role, content, finish string, hasFinish bool) {
	raw := appendChatCompletionChunkWithUsage(s.chunkScratch[:0], s.id, time.Now().Unix(), s.model, s.fingerprint, role, content, finish, hasFinish, s.finalUsage)
	s.chunkScratch = raw[:0]
	s.emitRaw(raw)
	s.sentAny = true
}

func (s *webChatStream) emitTextChunk(content string) {
	if s.toolStreamMode {
		return
	}
	if content != "" {
		s.emitChunk("", content, "", false)
	}
}

func (s *webChatStream) emitReasoningChunk(content string) {
	if content == "" {
		return
	}
	raw := appendChatCompletionReasoningChunk(s.chunkScratch[:0], s.id, time.Now().Unix(), s.model, s.fingerprint, content)
	s.chunkScratch = raw[:0]
	s.emitRaw(raw)
	s.sentAny = true
}

// emitCleanText sends answer text after stripping grok.com render markup.
func (s *webChatStream) emitCleanText(raw string) {
	if raw == "" || s.toolStreamMode {
		return
	}
	if s.markup == nil {
		if cleaned := sanitizeUpstreamText(raw); cleaned != "" {
			s.emitTextChunk(cleaned)
		}
		return
	}
	if cleaned := s.markup.feed(raw); cleaned != "" {
		s.emitTextChunk(cleaned)
	}
}

// emitImageURL sends an image found in the stream as Markdown, once, holding a
// -part-0 preview back until it is clear no full variant follows.
func (s *webChatStream) emitImageURL(raw string) {
	if !s.enableMediaExtraction {
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	if strings.Contains(raw, "-part-0/") {
		full := strings.ReplaceAll(raw, "-part-0/", "/")
		if s.seenFull[full] {
			return
		}
		s.pendingPart[full] = raw
		return
	}
	s.seenFull[raw] = true
	if s.emitted[raw] {
		return
	}
	val, errV := s.h.imageOutputValue(context.Background(), s.token, raw, "url")
	if errV != nil || strings.TrimSpace(val) == "" {
		val = raw
	}
	if s.publicBase != "" && strings.HasPrefix(val, "/") {
		val = s.publicBase + val
	}
	if md := formatImageMarkdown(val); md != "" {
		s.finalSnapshotMarkdown = append(s.finalSnapshotMarkdown, md)
		s.emitChunk("", md, "", false)
		s.emitted[raw] = true
	}
}

func (s *webChatStream) emitToolCallsChunk(content string, toolCalls []map[string]any, finish string, hasFinish bool) {
	if strings.TrimSpace(content) != "" {
		s.emitChunk("", strings.TrimSpace(content), "", false)
	}
	if len(toolCalls) == 0 {
		return
	}
	indexedToolCalls := make([]map[string]any, 0, len(toolCalls))
	for _, tc := range toolCalls {
		if tc == nil {
			continue
		}
		cp := make(map[string]any, len(tc)+1)
		maps.Copy(cp, tc)
		if _, ok := cp["index"]; !ok {
			cp["index"] = s.toolCallIndex
		}
		s.toolCallIndex++
		indexedToolCalls = append(indexedToolCalls, cp)
	}
	if len(indexedToolCalls) == 0 {
		return
	}
	raw := appendChatCompletionToolCallsChunkWithUsage(s.chunkScratch[:0], s.id, time.Now().Unix(), s.model, s.fingerprint, indexedToolCalls, finish, hasFinish, s.finalUsage)
	s.chunkScratch = raw[:0]
	s.emitRaw(raw)
	s.sentAny = true
}

func (s *webChatStream) onToolStreamText(text string) { s.emitChunk("", text, "", false) }

func (s *webChatStream) onToolStreamCall(toolCall map[string]any) {
	s.emitToolCallsChunk("", []map[string]any{toolCall}, "", false)
}

// --- upstream lines -----------------------------------------------------------

// onUpstream handles one grok.com response line.
func (s *webChatStream) onUpstream(resp map[string]any) error {
	if s.downstreamWriteErr != nil {
		return s.downstreamWriteErr
	}
	if s.logger != nil {
		if raw, err := json.Marshal(resp); err == nil {
			s.logger.LogOutputSSE("response", string(raw))
		}
	}
	mr := extractUpstreamModelResponse(resp)
	if s.fingerprint == "" {
		s.fingerprint = extractUpstreamFingerprint(resp, mr)
	}
	if rid := extractUpstreamResponseID(resp, mr); rid != "" {
		s.id = rid
	}
	if !s.sentRole {
		s.emitChunk("assistant", "", "", false)
		s.sentRole = true
	}
	tokenDelta := extractUpstreamTokenDelta(resp, mr)
	if isThinkingResponse(resp) {
		s.onThinking(tokenDelta, mr)
		return nil
	}
	if tokenDelta != "" {
		if s.textRefCollector != nil {
			s.textRefCollector.feed(tokenDelta)
		}
		if s.toolStreamMode {
			if s.toolPump != nil {
				s.toolPump.feed(tokenDelta, s.onToolStreamText, s.onToolStreamCall)
			}
		} else if !s.sawModelMessage {
			s.tokenFallback.WriteString(tokenDelta)
		}
	}
	if mr != nil {
		s.onModelResponse(mr)
	}
	// Broader fallback: sometimes URLs live outside modelResponse.
	if s.enableMediaExtraction {
		forEachImageCandidateFromValue(resp, true, true, 120, s.emitImageURL)
	}
	if s.spec.IsVideo {
		s.onVideoProgress(resp)
	}
	return nil
}

func (s *webChatStream) onThinking(tokenDelta string, mr map[string]any) {
	thinkingDelta := tokenDelta
	if thinkingDelta == "" && mr != nil {
		if snapshot := extractUpstreamMessage(mr); snapshot != "" && snapshot != s.lastThinkingMessage {
			thinkingDelta = streamMessageDelta(s.lastThinkingMessage, snapshot)
			s.lastThinkingMessage = snapshot
		}
	}
	if thinkingDelta != "" {
		s.reasoningContent.WriteString(thinkingDelta)
		s.emitReasoningChunk(thinkingDelta)
	}
}

// onModelResponse takes a message snapshot. Only the prefix two consecutive
// snapshots agree on is emitted, because grok.com may still revise the tail.
func (s *webChatStream) onModelResponse(mr map[string]any) {
	if msg := extractUpstreamMessage(mr); strings.TrimSpace(msg) != "" && msg != s.lastMessage {
		s.lastMessage = msg
		s.sawModelMessage = true
		if s.textRefCollector != nil {
			s.textRefCollector.feed(msg)
		}
		if s.pendingModelMessage != "" {
			stable := commonPrefixText(s.pendingModelMessage, msg)
			if stable != "" && stable != s.emittedModelMessage && strings.HasPrefix(stable, s.emittedModelMessage) {
				if delta := streamMessageDelta(s.emittedModelMessage, stable); delta != "" {
					s.emitCleanText(delta)
					s.emittedModelMessage = stable
				}
			}
		}
		s.pendingModelMessage = msg
		if s.toolStreamMode && s.toolPump != nil && !s.toolPump.SawEvents && !s.toolPump.EmittedToolCalls {
			textContent, toolCalls := s.toolParser.parseCalls(strings.TrimSpace(msg))
			if len(toolCalls) > 0 {
				s.emitToolCallsChunk(textContent, toolCalls, "", false)
				s.toolPump.EmittedToolCalls = true
				s.emittedModelMessage = msg
			}
		}
		if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
			slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
		}
	}
	if s.enableMediaExtraction {
		forEachImageCandidateFromValue(mr, true, true, 80, s.emitImageURL)
	}
}

func (s *webChatStream) onVideoProgress(resp map[string]any) {
	progress, videoURL, ok := extractVideoProgress(resp)
	if !ok {
		return
	}
	if progress > 0 && progress < 100 {
		s.emitChunk("", fmt.Sprintf("正在生成视频中，当前进度%d%%\n", progress), "", false)
	}
	if progress >= 100 && strings.TrimSpace(videoURL) == "" {
		videoURL = firstAssetURL(resp)
	}
	if progress >= 100 && strings.TrimSpace(videoURL) != "" {
		finalURL := strings.TrimSpace(videoURL)
		if name, err := s.h.cacheMediaURL(context.Background(), s.token, finalURL, "video"); err == nil && name != "" {
			finalURL = "/grok/v1/files/video/" + name
		}
		if s.publicBase != "" && strings.HasPrefix(finalURL, "/") {
			finalURL = s.publicBase + finalURL
		}
		s.emitChunk("", finalURL, "", false)
	}
}

// --- end of stream ------------------------------------------------------------

// fail ends a stream the upstream or the client broke. A stalled upstream is
// not a parse error: the idle sentinel gets its own text and code.
func (s *webChatStream) fail(err error) {
	slog.Warn("grok stream parse failed", "error", err)
	if errors.Is(err, ErrGrokSemanticIdle) {
		writeChatStreamError(s.w, s.flusher, s.logger, "upstream stream timed out while waiting for generated output", "upstream_stream_idle_timeout")
		return
	}
	writeChatStreamError(s.w, s.flusher, s.logger, "stream parse error: "+err.Error(), "stream_error")
}

// flushText sends the text still held back when the upstream ended, and
// returns what a tool-mode stream buffered instead of sending.
func (s *webChatStream) flushText() (buffered string) {
	tail := ""
	if s.pendingModelMessage != "" && s.pendingModelMessage != s.emittedModelMessage && strings.HasPrefix(s.pendingModelMessage, s.emittedModelMessage) {
		tail = streamMessageDelta(s.emittedModelMessage, s.pendingModelMessage)
	}
	if s.markup != nil {
		if tail != "" {
			s.emitCleanText(tail)
		}
		if !s.sawModelMessage && s.tokenFallback.Len() > 0 {
			s.emitCleanText(s.tokenFallback.String())
		}
		if rest := s.markup.flush(); rest != "" {
			s.emitTextChunk(rest)
		}
		if s.tokenFallback.Len() > 0 && !s.sawModelMessage && s.h != nil && s.h.configSnapshot() != nil && s.h.configSnapshot().DebugEnabled {
			slog.Debug("grok stream fallback used token deltas (no modelResponse)", "model", s.model)
		}
		return ""
	}
	text := ""
	if tail != "" {
		text = tail
	} else if !s.sawModelMessage && s.tokenFallback.Len() > 0 {
		text = s.tokenFallback.String()
	}
	if cleaned := sanitizeUpstreamText(text); cleaned != "" {
		if s.toolStreamMode {
			return cleaned
		}
		s.emitTextChunk(cleaned)
	}
	return ""
}

// flushMedia sends previews whose full variant never appeared (trying the full
// one through the media cache first), then image references found in text.
func (s *webChatStream) flushMedia() {
	if !s.enableMediaExtraction {
		return
	}
	for full, part := range s.pendingPart {
		if s.seenFull[full] {
			continue
		}
		if name, err := s.h.cacheMediaURL(context.Background(), s.token, full, "image"); err == nil && name != "" {
			val := "/grok/v1/files/image/" + name
			if s.publicBase != "" && strings.HasPrefix(val, "/") {
				val = s.publicBase + val
			}
			if md := formatImageMarkdown(val); md != "" {
				s.emitChunk("", md, "", false)
			}
			continue
		}
		s.emitImageURL(part)
	}
	s.textRefCollector.emit(s.emitImageURL)
}

// finish flushes what was held back and sends the final chunk with usage.
func (s *webChatStream) finish() {
	finalBufferedText := s.flushText()
	finalUsageText := strings.TrimSpace(finalBufferedText)
	finalSnapshotContent := ""
	if !s.toolStreamMode {
		switch {
		case strings.TrimSpace(s.pendingModelMessage) != "":
			finalSnapshotContent = sanitizeUpstreamText(s.pendingModelMessage)
		case s.tokenFallback.Len() > 0:
			finalSnapshotContent = sanitizeUpstreamText(s.tokenFallback.String())
		}
		if len(s.finalSnapshotMarkdown) > 0 {
			finalSnapshotContent += strings.Join(s.finalSnapshotMarkdown, "")
		}
		if finalUsageText == "" {
			finalUsageText = strings.TrimSpace(finalSnapshotContent)
		}
	}
	s.flushMedia()

	if s.toolStreamMode && s.finishToolCalls(finalBufferedText) {
		return
	}
	s.finalUsage = grokbase.AddReasoningUsage(buildChatUsagePayload(s.req, finalUsageText, nil), s.reasoningContent.String())
	if s.finalSnapshotEnabled && finalSnapshotContent != "" {
		raw := appendChatCompletionSnapshotChunkWithUsage(s.chunkScratch[:0], s.id, time.Now().Unix(), s.model, s.fingerprint, finalSnapshotContent, "stop", true, s.finalUsage)
		s.chunkScratch = raw[:0]
		s.emitRaw(raw)
	} else {
		s.emitChunk("", "", "stop", true)
	}
	s.emitRaw([]byte("[DONE]"))
}

// finishToolCalls ends a tool-mode stream that produced calls and reports
// whether it did.
func (s *webChatStream) finishToolCalls(buffered string) bool {
	if s.toolPump != nil {
		s.toolPump.flush(s.onToolStreamText, s.onToolStreamCall)
	}
	if s.toolPump != nil && s.toolPump.EmittedToolCalls {
		s.finalUsage = grokbase.AddReasoningUsage(buildChatUsagePayload(s.req, strings.TrimSpace(buffered), []map[string]any{{"type": "function"}}), s.reasoningContent.String())
		s.emitChunk("", "", "tool_calls", true)
		s.emitRaw([]byte("[DONE]"))
		return true
	}
	textContent, toolCalls := s.toolParser.parseCalls(strings.TrimSpace(buffered))
	textContent = strings.TrimSpace(textContent)
	if len(toolCalls) > 0 {
		s.finalUsage = grokbase.AddReasoningUsage(buildChatUsagePayload(s.req, textContent, toolCalls), s.reasoningContent.String())
		s.emitToolCallsChunk(textContent, toolCalls, "tool_calls", true)
		s.emitRaw([]byte("[DONE]"))
		return true
	}
	return false
}
