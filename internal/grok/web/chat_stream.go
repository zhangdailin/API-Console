package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"strings"
	"time"
)

func streamMessageDelta(previous, current string) string {
	if current == "" || current == previous {
		return ""
	}
	if previous == "" {
		return current
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	prevRunes := []rune(previous)
	currRunes := []rune(current)
	maxPrefix := len(prevRunes)
	if len(currRunes) < maxPrefix {
		maxPrefix = len(currRunes)
	}
	prefix := 0
	for prefix < maxPrefix && prevRunes[prefix] == currRunes[prefix] {
		prefix++
	}
	if prefix >= len(currRunes) {
		return ""
	}
	return string(currRunes[prefix:])
}

func commonPrefixText(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ar := []rune(a)
	br := []rune(b)
	limit := len(ar)
	if len(br) < limit {
		limit = len(br)
	}
	idx := 0
	for idx < limit && ar[idx] == br[idx] {
		idx++
	}
	if idx == 0 {
		return ""
	}
	return string(ar[:idx])
}

func toolCallsEnabled(tools []ToolDef, toolChoice interface{}) bool {
	if len(tools) == 0 {
		return false
	}
	if s, ok := toolChoice.(string); ok && strings.EqualFold(strings.TrimSpace(s), "none") {
		return false
	}
	return true
}

func suffixPrefixOverlap(text, tag string) int {
	if text == "" || tag == "" {
		return 0
	}
	maxKeep := len(text)
	if limit := len(tag) - 1; maxKeep > limit {
		maxKeep = limit
	}
	for keep := maxKeep; keep > 0; keep-- {
		if strings.HasSuffix(text, tag[:keep]) {
			return keep
		}
	}
	return 0
}

func (h *Handler) streamChat(w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice interface{}, body io.Reader, logger *debug.Logger) {
	flusher := streamResponseHeaders(w)

	id := "chatcmpl_" + randomHex(8)
	fingerprint := ""
	sentRole := false
	lastMessage := ""
	sentAny := false
	var finalUsage map[string]interface{}
	defer func() {
		if len(finalUsage) > 0 {
			h.auditChatOutcome(context.Background(), req.account, req, chatOutcome{Finish: "stop", Usage: finalUsage, UsageSource: audit.UsageSourceEstimated})
		}
	}()
	enableMediaExtraction := hasAttachments || spec.IsVideo
	// Image URL stream handling: prefer full image variants over -part-0 previews.
	seenFull := map[string]bool(nil)
	pendingPart := map[string]string(nil)
	emitted := map[string]bool(nil)
	if enableMediaExtraction {
		seenFull = map[string]bool{}
		pendingPart = map[string]string{}
		emitted = map[string]bool{}
	}
	sawModelMessage := false
	var tokenFallback strings.Builder
	var reasoningContent strings.Builder
	lastThinkingMessage := ""
	emittedModelMessage := ""
	pendingModelMessage := ""
	toolStreamMode := toolCallsEnabled(tools, toolChoice)
	toolParser := newToolCallParser(tools, toolChoice)
	finalSnapshotEnabled := !toolStreamMode && !spec.IsVideo
	toolCallIndex := 0
	var toolPump *toolStreamPump
	if toolStreamMode {
		toolPump = newToolStreamPump(toolParser)
	}
	finalSnapshotMarkdown := make([]string, 0, 4)

	var mf *streamMarkupFilter
	if !hasAttachments && !toolStreamMode {
		mf = &streamMarkupFilter{}
	}
	var textRefCollector *streamTextImageRefCollector
	if enableMediaExtraction {
		textRefCollector = newStreamTextImageRefCollector()
	}
	chunkScratch := make([]byte, 0, 256)
	var downstreamWriteErr error
	emitRaw := func(raw []byte) {
		if downstreamWriteErr == nil {
			downstreamWriteErr = writeSSELog(w, flusher, logger, raw)
		}
	}

	emitChunk := func(role, content string, finish string, hasFinish bool) {
		raw := appendChatCompletionChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, role, content, finish, hasFinish, finalUsage)
		chunkScratch = raw[:0]
		emitRaw(raw)
		sentAny = true
	}

	emitTextChunk := func(content string) {
		if toolStreamMode {
			return
		}
		if content != "" {
			emitChunk("", content, "", false)
		}
	}

	emitReasoningChunk := func(content string) {
		if content == "" {
			return
		}
		raw := appendChatCompletionReasoningChunk(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, content)
		chunkScratch = raw[:0]
		emitRaw(raw)
		sentAny = true
	}

	emitCleanText := func(raw string) {
		if raw == "" {
			return
		}
		if toolStreamMode {
			return
		}
		if mf == nil {
			if cleaned := sanitizeUpstreamText(raw); cleaned != "" {
				emitTextChunk(cleaned)
			}
			return
		}
		if cleaned := mf.feed(raw); cleaned != "" {
			emitTextChunk(cleaned)
		}
	}

	emitImageURL := func(raw string) {
		if !enableMediaExtraction {
			return
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		// Track preview vs full variants on the raw upstream URL.
		if strings.Contains(raw, "-part-0/") {
			full := strings.ReplaceAll(raw, "-part-0/", "/")
			if seenFull[full] {
				return
			}
			pendingPart[full] = raw
			return
		}
		seenFull[raw] = true
		if emitted[raw] {
			return
		}

		val, errV := h.imageOutputValue(context.Background(), token, raw, "url")
		if errV != nil || strings.TrimSpace(val) == "" {
			val = raw
		}
		if publicBase != "" && strings.HasPrefix(val, "/") {
			val = publicBase + val
		}
		if md := formatImageMarkdown(val); md != "" {
			finalSnapshotMarkdown = append(finalSnapshotMarkdown, md)
			emitChunk("", md, "", false)
			emitted[raw] = true
		}
	}

	emitToolCallsChunk := func(content string, toolCalls []map[string]interface{}, finish string, hasFinish bool) {
		if strings.TrimSpace(content) != "" {
			emitChunk("", strings.TrimSpace(content), "", false)
		}
		if len(toolCalls) == 0 {
			return
		}
		indexedToolCalls := make([]map[string]interface{}, 0, len(toolCalls))
		for _, tc := range toolCalls {
			if tc == nil {
				continue
			}
			cp := make(map[string]interface{}, len(tc)+1)
			for k, v := range tc {
				cp[k] = v
			}
			if _, ok := cp["index"]; !ok {
				cp["index"] = toolCallIndex
			}
			toolCallIndex++
			indexedToolCalls = append(indexedToolCalls, cp)
		}
		if len(indexedToolCalls) == 0 {
			return
		}
		raw := appendChatCompletionToolCallsChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, indexedToolCalls, finish, hasFinish, finalUsage)
		chunkScratch = raw[:0]
		emitRaw(raw)
		sentAny = true
	}

	onToolStreamText := func(s string) {
		emitChunk("", s, "", false)
	}
	onToolStreamCall := func(toolCall map[string]interface{}) {
		emitToolCallsChunk("", []map[string]interface{}{toolCall}, "", false)
	}
	handleToolStreamChunk := func(chunk string) {
		if toolPump != nil {
			toolPump.feed(chunk, onToolStreamText, onToolStreamCall)
		}
	}
	flushToolStreamChunk := func() {
		if toolPump != nil {
			toolPump.flush(onToolStreamText, onToolStreamCall)
		}
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if downstreamWriteErr != nil {
			return downstreamWriteErr
		}
		if logger != nil {
			if raw, err := json.Marshal(resp); err == nil {
				logger.LogOutputSSE("response", string(raw))
			}
		}
		mr := extractUpstreamModelResponse(resp)
		if fingerprint == "" {
			fingerprint = extractUpstreamFingerprint(resp, mr)
		}
		if rid := extractUpstreamResponseID(resp, mr); rid != "" {
			id = rid
		}
		if !sentRole {
			emitChunk("assistant", "", "", false)
			sentRole = true
		}
		tokenDelta := extractUpstreamTokenDelta(resp, mr)
		if isThinkingResponse(resp) {
			thinkingDelta := tokenDelta
			if thinkingDelta == "" && mr != nil {
				if snapshot := extractUpstreamMessage(mr); snapshot != "" && snapshot != lastThinkingMessage {
					thinkingDelta = streamMessageDelta(lastThinkingMessage, snapshot)
					lastThinkingMessage = snapshot
				}
			}
			if thinkingDelta != "" {
				reasoningContent.WriteString(thinkingDelta)
				emitReasoningChunk(thinkingDelta)
			}
			return nil
		}
		if tokenDelta != "" {
			if textRefCollector != nil {
				textRefCollector.feed(tokenDelta)
			}
			if toolStreamMode {
				handleToolStreamChunk(tokenDelta)
			} else if !sawModelMessage {
				tokenFallback.WriteString(tokenDelta)
			}
		}
		if mr != nil {
			if msg := extractUpstreamMessage(mr); strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				sawModelMessage = true
				if textRefCollector != nil {
					textRefCollector.feed(msg)
				}
				if pendingModelMessage != "" {
					stable := commonPrefixText(pendingModelMessage, msg)
					if stable != "" && stable != emittedModelMessage && strings.HasPrefix(stable, emittedModelMessage) {
						if delta := streamMessageDelta(emittedModelMessage, stable); delta != "" {
							emitCleanText(delta)
							emittedModelMessage = stable
						}
					}
				}
				pendingModelMessage = msg
				if toolStreamMode && toolPump != nil && !toolPump.SawEvents && !toolPump.EmittedToolCalls {
					textContent, toolCalls := toolParser.parseCalls(strings.TrimSpace(msg))
					if len(toolCalls) > 0 {
						emitToolCallsChunk(textContent, toolCalls, "", false)
						if toolPump != nil {
							toolPump.EmittedToolCalls = true
						}
						emittedModelMessage = msg
					}
				}
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			if enableMediaExtraction {
				forEachImageCandidateFromValue(mr, true, true, 80, emitImageURL)
			}
		}
		// Broader fallback: sometimes URLs live outside modelResponse.
		if enableMediaExtraction {
			forEachImageCandidateFromValue(resp, true, true, 120, emitImageURL)
		}
		if spec.IsVideo {
			if progress, videoURL, ok := extractVideoProgress(resp); ok {
				if progress > 0 && progress < 100 {
					emitChunk("", fmt.Sprintf("正在生成视频中，当前进度%d%%\n", progress), "", false)
				}
				if progress >= 100 && strings.TrimSpace(videoURL) == "" {
					videoURL = firstAssetURL(resp)
				}
				if progress >= 100 && strings.TrimSpace(videoURL) != "" {
					finalURL := strings.TrimSpace(videoURL)
					if name, err := h.cacheMediaURL(context.Background(), token, finalURL, "video"); err == nil && name != "" {
						finalURL = "/grok/v1/files/video/" + name
					}
					if publicBase != "" && strings.HasPrefix(finalURL, "/") {
						finalURL = publicBase + finalURL
					}
					emitChunk("", finalURL, "", false)
				}
			}
		}
		return nil
	})
	if err == nil && downstreamWriteErr != nil {
		err = downstreamWriteErr
	}
	if err != nil {
		slog.Warn("grok stream parse failed", "error", err)
		if !sentAny {
			// A stalled upstream is not a parse error: the idle watchdog's sentinel
			// gets its own text and its own code so a client can tell the two apart.
			if errors.Is(err, ErrGrokSemanticIdle) {
				writeChatStreamError(w, flusher, logger, "upstream stream timed out while waiting for generated output", "upstream_stream_idle_timeout")
			} else {
				writeChatStreamError(w, flusher, logger, "stream parse error: "+err.Error(), "stream_error")
			}
			return
		}
		// A stalled upstream is not a parse error: the idle watchdog's sentinel
		// gets its own text and its own code so a client can tell the two apart.
		if errors.Is(err, ErrGrokSemanticIdle) {
			writeChatStreamError(w, flusher, logger, "upstream stream timed out while waiting for generated output", "upstream_stream_idle_timeout")
		} else {
			writeChatStreamError(w, flusher, logger, "stream parse error: "+err.Error(), "stream_error")
		}
		return
	}

	finalBufferedText := ""
	finalSnapshotContent := ""
	// Flush any remaining buffered text (avoids "no content" when stream ends quickly).
	if mf != nil {
		if pendingModelMessage != "" && pendingModelMessage != emittedModelMessage && strings.HasPrefix(pendingModelMessage, emittedModelMessage) {
			if delta := streamMessageDelta(emittedModelMessage, pendingModelMessage); delta != "" {
				emitCleanText(delta)
			}
		}
		if !sawModelMessage && tokenFallback.Len() > 0 {
			emitCleanText(tokenFallback.String())
		}
		if tail := mf.flush(); tail != "" {
			emitTextChunk(tail)
		}
		if tokenFallback.Len() > 0 && !sawModelMessage && h != nil && h.configSnapshot() != nil && h.configSnapshot().DebugEnabled {
			slog.Debug("grok stream fallback used token deltas (no modelResponse)", "model", model)
		}
	} else {
		if pendingModelMessage != "" && pendingModelMessage != emittedModelMessage && strings.HasPrefix(pendingModelMessage, emittedModelMessage) {
			if delta := streamMessageDelta(emittedModelMessage, pendingModelMessage); delta != "" {
				if cleaned := sanitizeUpstreamText(delta); cleaned != "" {
					if toolStreamMode {
						finalBufferedText += cleaned
					} else {
						emitTextChunk(cleaned)
					}
				}
			}
		} else if !sawModelMessage && tokenFallback.Len() > 0 {
			if cleaned := sanitizeUpstreamText(tokenFallback.String()); cleaned != "" {
				if toolStreamMode {
					finalBufferedText += cleaned
				} else {
					emitTextChunk(cleaned)
				}
			}
		}
	}
	finalUsageText := strings.TrimSpace(finalBufferedText)
	if !toolStreamMode {
		switch {
		case strings.TrimSpace(pendingModelMessage) != "":
			finalSnapshotContent = sanitizeUpstreamText(pendingModelMessage)
		case tokenFallback.Len() > 0:
			finalSnapshotContent = sanitizeUpstreamText(tokenFallback.String())
		}
		if len(finalSnapshotMarkdown) > 0 {
			finalSnapshotContent += strings.Join(finalSnapshotMarkdown, "")
		}
		if finalUsageText == "" {
			finalUsageText = strings.TrimSpace(finalSnapshotContent)
		}
	}
	// Emit any pending part-0 previews only if we never saw a full variant.
	// Try to fetch/emit the full variant first; if it doesn't exist, fall back to the preview.
	if enableMediaExtraction {
		for full, part := range pendingPart {
			if seenFull[full] {
				continue
			}
			// Try full (cache through this server for client reachability).
			if name, err := h.cacheMediaURL(context.Background(), token, full, "image"); err == nil && name != "" {
				val := "/grok/v1/files/image/" + name
				if publicBase != "" && strings.HasPrefix(val, "/") {
					val = publicBase + val
				}
				if md := formatImageMarkdown(val); md != "" {
					emitChunk("", md, "", false)
				}
				continue
			}
			// Fall back to preview.
			emitImageURL(part)
		}

		// Final pass: emit URL/path candidates found in incremental text collector.
		textRefCollector.emit(emitImageURL)
	}

	if toolStreamMode {
		flushToolStreamChunk()
		if toolPump != nil && toolPump.EmittedToolCalls {
			finalUsage = addReasoningUsage(buildChatUsagePayload(req, strings.TrimSpace(finalBufferedText), []map[string]interface{}{{"type": "function"}}), reasoningContent.String())
			emitChunk("", "", "tool_calls", true)
			emitRaw([]byte("[DONE]"))
			return
		}
		textContent, toolCalls := toolParser.parseCalls(strings.TrimSpace(finalBufferedText))
		textContent = strings.TrimSpace(textContent)
		if len(toolCalls) > 0 {
			finalUsage = addReasoningUsage(buildChatUsagePayload(req, textContent, toolCalls), reasoningContent.String())
			emitToolCallsChunk(textContent, toolCalls, "tool_calls", true)
			emitRaw([]byte("[DONE]"))
			return
		}
	}

	finalUsage = addReasoningUsage(buildChatUsagePayload(req, finalUsageText, nil), reasoningContent.String())
	if finalSnapshotEnabled && finalSnapshotContent != "" {
		raw := appendChatCompletionSnapshotChunkWithUsage(chunkScratch[:0], id, time.Now().Unix(), model, fingerprint, finalSnapshotContent, "stop", true, finalUsage)
		chunkScratch = raw[:0]
		emitRaw(raw)
	} else {
		emitChunk("", "", "stop", true)
	}
	emitRaw([]byte("[DONE]"))
}

func (h *Handler) collectChat(w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice interface{}, body io.Reader, logger *debug.Logger) {
	id := "chatcmpl_" + randomHex(8)
	lastMessage := ""
	videoURL := ""
	fingerprint := ""
	enableMediaExtraction := hasAttachments || spec.IsVideo
	imageCandidates := make([]string, 0, 8)
	imageCandidateSet := map[string]struct{}{}
	var tokenContent strings.Builder
	var reasoningTokens strings.Builder
	reasoningSnapshot := ""
	addImageCandidate := func(raw string) {
		u := strings.TrimSpace(raw)
		if u == "" {
			return
		}
		if _, ok := imageCandidateSet[u]; ok {
			return
		}
		imageCandidateSet[u] = struct{}{}
		imageCandidates = append(imageCandidates, u)
	}

	err := parseUpstreamLines(body, func(resp map[string]interface{}) error {
		if logger != nil {
			if raw, err := json.Marshal(resp); err == nil {
				logger.LogOutputSSE("response", string(raw))
			}
		}
		mr := extractUpstreamModelResponse(resp)
		if fingerprint == "" {
			fingerprint = extractUpstreamFingerprint(resp, mr)
		}
		if rid := extractUpstreamResponseID(resp, mr); rid != "" {
			id = rid
		}
		isThinking := isThinkingResponse(resp)
		tokenDelta := extractUpstreamTokenDelta(resp, mr)
		if isThinking {
			if tokenDelta != "" {
				reasoningTokens.WriteString(tokenDelta)
			}
			if mr != nil {
				if snapshot := extractUpstreamMessage(mr); strings.TrimSpace(snapshot) != "" {
					reasoningSnapshot = snapshot
				}
			}
			return nil
		}
		if tokenDelta != "" {
			tokenContent.WriteString(tokenDelta)
		}
		if mr != nil {
			if msg := extractUpstreamMessage(mr); strings.TrimSpace(msg) != "" && msg != lastMessage {
				lastMessage = msg
				if strings.Contains(msg, "<grok:render") || strings.Contains(msg, "tool_usage_card") {
					slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
				}
			}
			if enableMediaExtraction {
				forEachImageCandidateFromValue(mr, true, false, 0, addImageCandidate)
			}
		}
		if enableMediaExtraction {
			forEachImageCandidateFromValue(resp, false, false, 0, addImageCandidate)
		}
		if spec.IsVideo {
			if progress, vurl, ok := extractVideoProgress(resp); ok && progress >= 100 {
				videoURL = strings.TrimSpace(vurl)
				if videoURL == "" {
					videoURL = firstAssetURL(resp)
				}
			}
		}
		return nil
	})
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}

	tokenClean := sanitizeUpstreamText(tokenContent.String())
	modelClean := sanitizeUpstreamText(lastMessage)

	finalContent := modelClean
	if strings.TrimSpace(finalContent) == "" {
		finalContent = tokenClean
	}
	reasoningContent := sanitizeUpstreamText(reasoningSnapshot)
	if strings.TrimSpace(reasoningContent) == "" {
		reasoningContent = sanitizeUpstreamText(reasoningTokens.String())
	}

	var toolCalls []map[string]interface{}
	if toolCallsEnabled(tools, toolChoice) {
		textContent, parsedToolCalls := newToolCallParser(tools, toolChoice).parseCalls(finalContent)
		finalContent = strings.TrimSpace(textContent)
		toolCalls = parsedToolCalls
	}

	if videoURL != "" {
		if name, err := h.cacheMediaURL(context.Background(), token, videoURL, "video"); err == nil && name != "" {
			videoURL = "/grok/v1/files/video/" + name
		}
		if publicBase != "" && strings.HasPrefix(videoURL, "/") {
			videoURL = publicBase + videoURL
		}
		if strings.TrimSpace(finalContent) != "" {
			finalContent += "\n"
		}
		finalContent += videoURL
	}

	// Append any collected image links as Markdown, after text cleanup.
	if enableMediaExtraction {
		imgs := normalizeImageURLs(imageCandidates, 8)
		for _, u := range imgs {
			val, errV := h.imageOutputValue(context.Background(), token, u, "url")
			if errV != nil || strings.TrimSpace(val) == "" {
				val = u
			}
			if publicBase != "" && strings.HasPrefix(val, "/") {
				val = publicBase + val
			}
			finalContent += formatImageMarkdown(val)
		}
	}
	message := map[string]interface{}{
		"role":        "assistant",
		"content":     finalContent,
		"refusal":     nil,
		"annotations": []interface{}{},
	}
	if strings.TrimSpace(reasoningContent) != "" {
		message["reasoning_content"] = reasoningContent
	}
	usage := addReasoningUsage(buildChatUsagePayload(req, finalContent, toolCalls), reasoningContent)
	resp := map[string]interface{}{
		"id":                 id,
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              model,
		"service_tier":       nil,
		"system_fingerprint": fingerprint,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	}
	if len(toolCalls) > 0 {
		choice := resp["choices"].([]map[string]interface{})[0]
		message := choice["message"].(map[string]interface{})
		message["tool_calls"] = toolCalls
		if strings.TrimSpace(finalContent) == "" {
			message["content"] = nil
		}
		choice["finish_reason"] = "tool_calls"
	}
	writeJSON(w, resp)
	h.auditChatOutcome(context.Background(), req.account, req, chatOutcome{Finish: "stop", Usage: usage, UsageSource: audit.UsageSourceEstimated})
}
