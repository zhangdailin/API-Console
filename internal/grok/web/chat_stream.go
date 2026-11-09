package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/util"
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
	maxPrefix := min(len(currRunes), len(prevRunes))
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
	limit := min(len(br), len(ar))
	idx := 0
	for idx < limit && ar[idx] == br[idx] {
		idx++
	}
	if idx == 0 {
		return ""
	}
	return string(ar[:idx])
}

func toolCallsEnabled(tools []ToolDef, toolChoice any) bool {
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

// streamChat relays a grok.com AppChat stream as Chat Completions chunks. The
// per-stream state and its emitters live on webChatStream
// (chat_stream_state.go); this function is the order of events: open, relay
// each upstream line, then flush what was buffered and finish.
func (h *Handler) streamChat(w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice any, body io.Reader, logger *debug.Logger) {
	s := newWebChatStream(h, w, req, model, spec, token, publicBase, hasAttachments, tools, toolChoice, logger)
	defer s.auditOutcome()

	err := parseUpstreamLines(body, s.onUpstream)
	if err == nil && s.downstreamWriteErr != nil {
		err = s.downstreamWriteErr
	}
	if err != nil {
		s.fail(err)
		return
	}
	s.finish()
}

func (h *Handler) collectChat(w http.ResponseWriter, req *ChatCompletionsRequest, model string, spec ModelSpec, token string, publicBase string, hasAttachments bool, tools []ToolDef, toolChoice any, body io.Reader, logger *debug.Logger) {
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

	err := parseUpstreamLines(body, func(resp map[string]any) error {
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

	var toolCalls []map[string]any
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
	message := map[string]any{
		"role":        "assistant",
		"content":     finalContent,
		"refusal":     nil,
		"annotations": []any{},
	}
	if strings.TrimSpace(reasoningContent) != "" {
		message["reasoning_content"] = reasoningContent
	}
	usage := grokbase.AddReasoningUsage(buildChatUsagePayload(req, finalContent, toolCalls), reasoningContent)
	resp := map[string]any{
		"id":                 id,
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              model,
		"service_tier":       nil,
		"system_fingerprint": fingerprint,
		"choices": []map[string]any{
			{
				"index":         0,
				"message":       message,
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	}
	if len(toolCalls) > 0 {
		choice := resp["choices"].([]map[string]any)[0]
		message := choice["message"].(map[string]any)
		message["tool_calls"] = toolCalls
		if strings.TrimSpace(finalContent) == "" {
			message["content"] = nil
		}
		choice["finish_reason"] = "tool_calls"
	}
	util.WriteJSON(w, resp)
	h.auditChatOutcome(context.Background(), req.account, req, chatOutcome{Finish: "stop", Usage: usage, UsageSource: audit.UsageSourceEstimated})
}
