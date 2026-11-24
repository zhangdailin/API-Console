package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
)

func streamMessageDelta(previous, current string) string {
	return current[sharedSnapshotPrefix(previous, current):]
}

func commonPrefixText(a, b string) string {
	return a[:sharedSnapshotPrefix(a, b)]
}

// JSON decoding supplies valid UTF-8. The usual growing snapshot takes the
// prefix check without allocating; a revised tail can split a rune's bytes.
func sharedSnapshotPrefix(a, b string) int {
	limit := min(len(a), len(b))
	if a[:limit] == b[:limit] {
		return limit
	}
	i := 0
	for i < limit && a[i] == b[i] {
		i++
	}
	for i > 0 && !utf8.RuneStart(a[i]) {
		i--
	}
	return i
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

type chatUpstreamFrame struct {
	raw, model    map[string]any
	delta, answer string
	thinking      bool
}

type chatResponseIdentity struct{ id, fingerprint string }

func newChatResponseIdentity() chatResponseIdentity {
	return chatResponseIdentity{id: "chatcmpl_" + util.RandomHex(8)}
}

func (i *chatResponseIdentity) observe(frame chatUpstreamFrame) {
	if i.fingerprint == "" {
		i.fingerprint = extractUpstreamFingerprint(frame.raw, frame.model)
	}
	if id := extractUpstreamResponseID(frame.raw, frame.model); id != "" {
		i.id = id
	}
}

func logChatMarkup(text string) {
	if strings.Contains(text, "<grok:render") || strings.Contains(text, "tool_usage_card") {
		slog.Debug("grok message contains render/tool markup", "has_modelResponse", true)
	}
}

var errIncompleteWebChat = errors.New("upstream conversation ended before a final answer")

// parseChatUpstream requires an answer snapshot or an explicit stop. A clean
// transport EOF can also follow a prefix of a syntactically valid NDJSON body.
func parseChatUpstream(body io.Reader, hasAttachments bool, handle func(chatUpstreamFrame) error) error {
	var complete, answer bool
	err := parseUpstreamLines(body, func(raw map[string]any) error {
		model := extractUpstreamModelResponse(raw)
		frame := chatUpstreamFrame{raw: raw, model: model, delta: extractUpstreamTokenDelta(raw, model), answer: extractUpstreamMessage(model), thinking: isThinkingResponse(raw)}
		if !frame.thinking {
			if strings.TrimSpace(frame.delta) != "" {
				complete, answer = false, true
			}
			if strings.TrimSpace(frame.answer) != "" || (hasAttachments && (len(extractImageURLs(model)) > 0 || len(extractAppChatImageURLs(raw)) > 0)) {
				complete, answer = true, true
			}
			if stopped, _ := raw["isSoftStop"].(bool); stopped && answer {
				complete = true
			}
		}
		return handle(frame)
	})
	if (err == nil && !complete) || errors.Is(err, io.ErrUnexpectedEOF) {
		return errIncompleteWebChat
	}
	return err
}

// streamChat relays a grok.com AppChat stream as Chat Completions chunks. The
// per-stream state and its emitters live on webChatStream
// (chat_stream_state.go); this function is the order of events: open, relay
// each upstream line, then flush what was buffered and finish.
func (h *Handler) streamChat(ctx context.Context, w http.ResponseWriter, req *ChatCompletionsRequest, token, publicBase string, hasAttachments bool, body io.Reader, logger *debug.Logger) {
	s := newWebChatStream(ctx, h, w, req, token, publicBase, hasAttachments, logger)
	defer s.auditOutcome()

	err := parseChatUpstream(body, hasAttachments, s.onUpstream)
	if ctx.Err() != nil {
		return
	}
	if err == nil {
		err = s.deliveryError()
	}
	if err != nil {
		s.fail(err)
		return
	}
	s.finish()
}

func (h *Handler) collectChat(ctx context.Context, w http.ResponseWriter, req *ChatCompletionsRequest, token, publicBase string, hasAttachments bool, body io.Reader, logger *debug.Logger) {
	identity := newChatResponseIdentity()
	lastMessage := ""
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

	err := parseChatUpstream(body, hasAttachments, func(frame chatUpstreamFrame) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		logger.LogOutputSSEJSON("response", frame.raw)
		identity.observe(frame)
		if frame.thinking {
			reasoningTokens.WriteString(frame.delta)
			if strings.TrimSpace(frame.answer) != "" {
				reasoningSnapshot = frame.answer
			}
			return nil
		}
		tokenContent.WriteString(frame.delta)
		if strings.TrimSpace(frame.answer) != "" && frame.answer != lastMessage {
			lastMessage = frame.answer
			logChatMarkup(lastMessage)
		}
		if hasAttachments {
			forEachImageCandidateFromValue(frame.model, true, false, 0, addImageCandidate)
			forEachImageCandidateFromValue(frame.raw, false, false, 0, addImageCandidate)
		}
		return nil
	})
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		h.auditChatOutcome(context.WithoutCancel(ctx), req.account, req, chatOutcome{Err: err})
		switch {
		case errors.Is(err, errIncompleteWebChat):
			httpserver.WriteErrorCode(w, http.StatusBadGateway, "upstream_stream_incomplete", err.Error())
		case isUpstreamFailure(err):
			writeGrokUpstreamError(w, err)
		default:
			httpserver.WriteErrorCode(w, http.StatusBadGateway, "upstream_stream_error", "Invalid upstream conversation stream")
		}
		return
	}

	finalContent := preferredChatText(lastMessage, &tokenContent)
	reasoningContent := preferredChatText(reasoningSnapshot, &reasoningTokens)

	var toolCalls []map[string]any
	if toolCallsEnabled(req.Tools, req.ToolChoice) {
		textContent, parsedToolCalls := newToolCallParser(req.Tools, req.ToolChoice).parseCalls(finalContent)
		finalContent = strings.TrimSpace(textContent)
		toolCalls = parsedToolCalls
	}

	// Append any collected image links as Markdown, after text cleanup.
	if hasAttachments {
		imgs := normalizeImageURLs(imageCandidates, 8)
		for _, u := range imgs {
			finalContent += h.chatImageMarkdown(ctx, token, publicBase, u)
			if ctx.Err() != nil {
				return
			}
		}
	}
	if ctx.Err() != nil {
		return
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
	finish := "stop"
	if len(toolCalls) > 0 {
		finish = "tool_calls"
		message["tool_calls"] = toolCalls
		if strings.TrimSpace(finalContent) == "" {
			message["content"] = nil
		}
	}
	resp := map[string]any{
		"id":                 identity.id,
		"object":             "chat.completion",
		"created":            time.Now().Unix(),
		"model":              req.Model,
		"service_tier":       nil,
		"system_fingerprint": identity.fingerprint,
		"choices":            []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		"usage":              usage,
	}
	util.WriteJSON(w, resp)
	h.auditChatOutcome(context.WithoutCancel(ctx), req.account, req, chatOutcome{Finish: "stop", Usage: usage, UsageSource: audit.UsageSourceEstimated})
}

// Prefer a usable sanitized snapshot over the assembled token deltas.
func preferredChatText(snapshot string, tokens *strings.Builder) string {
	if clean := sanitizeUpstreamText(snapshot); strings.TrimSpace(clean) != "" {
		return clean
	}
	return sanitizeUpstreamText(tokens.String())
}

func (h *Handler) chatImageMarkdown(ctx context.Context, token, publicBase, raw string) string {
	value, err := h.imageOutputValue(ctx, token, raw, "url")
	if err != nil || strings.TrimSpace(value) == "" {
		value = raw
	}
	return chatImageLink(value, publicBase)
}

func chatImageLink(value, publicBase string) string {
	if strings.HasPrefix(value, "/") {
		value = publicBase + value
	}
	return formatImageMarkdown(value)
}
