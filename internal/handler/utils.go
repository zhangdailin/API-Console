package handler

import (
	"net/http"
	"slices"
	"strings"
	"unicode"

	"orchids-api/internal/channel"
	"orchids-api/internal/middleware"
	"orchids-api/internal/prompt"
)

func channelFromPath(path string) string {
	if id, ok := channel.FromPath(path); ok {
		return string(id)
	}
	return ""
}

// passthroughChannelName reports the canonical name of a passthrough channel —
// WorkBuddy, Qoder or Cline — and "" for every other name. Those three forward
// the caller's OpenAI-shaped request upstream verbatim.
// passthroughChannelName resolves a name to the channel that forwards the
// caller's OpenAI-shaped request upstream verbatim.
//
// The answer comes from the channel table rather than a local list: "generic" is
// the property that says a channel has no protocol of its own, so a channel
// added to internal/channel is picked up here without this file changing.
func passthroughChannelName(name string) string {
	id, ok := channel.Parse(name)
	if !ok {
		return ""
	}
	definition, ok := channel.DefinitionFor(id)
	if !ok || !definition.Generic {
		return ""
	}
	return string(id)
}

// mapModel normalizes only syntax. Availability and upstream identity come from
// the discovered Store.Model row; unknown IDs are never rewritten to a compiled
// fallback model.
func mapModel(requestModel string) string {
	normalized := strings.ToLower(strings.TrimSpace(requestModel))
	if strings.HasPrefix(normalized, "claude-") {
		normalized = strings.ReplaceAll(normalized, "4.6", "4-6")
		normalized = strings.ReplaceAll(normalized, "4.5", "4-5")
	}
	return normalized
}

func conversationKeyForRequest(r *http.Request, req ClaudeRequest) string {
	if req.ConversationID != "" {
		return req.ConversationID
	}
	if req.ConversationIDAlt != "" {
		return req.ConversationIDAlt
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "conversation_id", "conversationId", "session_id", "sessionId", "thread_id", "threadId", "chat_id", "chatId"); key != "" {
			return key
		}
	}
	if key := headerValue(r, "X-Conversation-Id", "X-Session-Id", "X-Thread-Id", "X-Chat-Id"); key != "" {
		return key
	}
	if req.Metadata != nil {
		if key := metadataString(req.Metadata, "user_id", "userId"); key != "" {
			return key
		}
	}
	return ""
}

// explicitConversationID returns only a client-supplied conversation id. It is
// intentionally narrower than conversationKeyForRequest: user/session fallback
// keys are useful for local routing but must not be presented to WorkBuddy as a
// real upstream conversation identifier.
func explicitConversationID(r *http.Request, req ClaudeRequest) string {
	if id := strings.TrimSpace(req.ConversationID); id != "" {
		return id
	}
	if id := strings.TrimSpace(req.ConversationIDAlt); id != "" {
		return id
	}
	if req.Metadata != nil {
		if id := metadataString(req.Metadata, "conversation_id", "conversationId"); id != "" {
			return id
		}
	}
	return headerValue(r, "X-Conversation-Id")
}

// workBuddyConversationRequestID is the turn-level aggregation key. Honour a
// client that already supplies one; otherwise the middleware-generated request
// ID is stable for every account switch and retry in this downstream request.
func workBuddyConversationRequestID(r *http.Request) string {
	if r == nil {
		return ""
	}
	if id := strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID")); id != "" {
		return id
	}
	return middleware.GetRequestID(r.Context())
}

func metadataString(metadata map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := metadata[key]; ok {
			if str, ok := value.(string); ok {
				str = strings.TrimSpace(str)
				if str != "" {
					return str
				}
			}
		}
	}
	return ""
}

func headerValue(r *http.Request, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(r.Header.Get(key)); value != "" {
			return value
		}
	}
	return ""
}

func extractUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "user" {
			if text := msg.ExtractText(); text != "" {
				return text
			}
		}
	}
	return ""
}

func lastUserIsToolResultFollowup(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if msg.Content.IsString() {
			return false
		}
		blocks := msg.Content.GetBlocks()
		hasToolResult := false
		for _, block := range blocks {
			switch block.Type {
			case "tool_result":
				hasToolResult = true
			case "text":
				continue
			default:
				if strings.TrimSpace(block.Type) != "" {
					return false
				}
			}
		}
		return hasToolResult
	}
	return false
}

func extractToolResultContent(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				if s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

const clientEmptyOutputRecoveryMarker = "[Your previous response had no visible output. Please continue and produce a user-visible response.]"

func isClientEmptyOutputRecoveryText(text string) bool {
	return strings.TrimSpace(text) == clientEmptyOutputRecoveryMarker
}

func emptyOutputRecoveryPrefix(messages []prompt.Message) ([]prompt.Message, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	last := messages[len(messages)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "user") ||
		!last.Content.IsString() ||
		!isClientEmptyOutputRecoveryText(last.Content.GetText()) {
		return messages, false
	}
	previous := messages[len(messages)-2]
	if !strings.EqualFold(strings.TrimSpace(previous.Role), "assistant") || strings.TrimSpace(previous.ExtractText()) != "" {
		return messages, false
	}
	if !previous.Content.IsString() {
		for _, block := range previous.Content.GetBlocks() {
			if block.Type != "text" || strings.TrimSpace(block.Text) != "" {
				return messages, false
			}
		}
	}
	return messages[:len(messages)-2], true
}

func successfulFileMutationToolResultFallback(messages []prompt.Message) string {
	effective, _ := emptyOutputRecoveryPrefix(messages)
	if len(effective) == 0 {
		return ""
	}

	toolNames := make(map[string]string)
	for _, msg := range effective {
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") || msg.Content.IsString() {
			continue
		}
		for _, block := range msg.Content.GetBlocks() {
			if block.Type == "tool_use" && strings.TrimSpace(block.ID) != "" {
				toolNames[block.ID] = normalizeToolNameKey(block.Name)
			}
		}
	}

	last := effective[len(effective)-1]
	if !strings.EqualFold(strings.TrimSpace(last.Role), "user") || last.Content.IsString() {
		return ""
	}
	count := 0
	for _, block := range last.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				return ""
			}
		case "tool_result":
			if block.IsError || looksLikeToolResultFailure(extractToolResultContent(block.Content)) {
				return ""
			}
			switch toolNames[block.ToolUseID] {
			case "write", "edit", "notebookedit":
				count++
			default:
				return ""
			}
		default:
			return ""
		}
	}
	if count == 0 {
		return ""
	}
	if count == 1 {
		return "File operation completed successfully."
	}
	return "Requested file operations completed successfully."
}

func lastNonToolResultUserText(messages []prompt.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), "user") {
			continue
		}
		if msg.Content.IsString() {
			text := strings.TrimSpace(stripSystemRemindersForMode(msg.Content.GetText()))
			if text != "" && !containsSuggestionMode(text) && !isClientEmptyOutputRecoveryText(text) {
				return text
			}
			continue
		}
		// Only text blocks carry a user turn here; a tool_result block is skipped
		// either way, so the walk simply moves on to the previous message.
		var parts []string
		for _, block := range msg.Content.GetBlocks() {
			if block.Type != "text" {
				continue
			}
			text := strings.TrimSpace(stripSystemRemindersForMode(block.Text))
			if text != "" && !containsSuggestionMode(text) {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return strings.TrimSpace(strings.Join(parts, "\n"))
		}
	}
	return ""
}

func looksLikeToolResultFailure(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"file does not exist",
		"no such file or directory",
		"no such file",
		"cannot find the file",
		"cannot open file",
		"permission denied",
		"is a directory",
		"current working directory is ",
		"file has not been read yet",
		"read it first before writing to it",
		"old_string not found",
		"string to replace not found",
		"could not find old_string",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isSuggestionMode(messages []prompt.Message) bool {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "user" {
			text := msg.ExtractText()
			if text != "" {
				return containsSuggestionMode(text)
			}
			return false
		}
	}
	return false
}

func buildLocalSuggestion(messages []prompt.Message) string {
	lastUser := lastNonSuggestionUserText(messages)
	lastAssistant := lastAssistantText(messages)
	if lastAssistant == "" || !hasExplicitNextStepOffer(lastAssistant) {
		return ""
	}
	if containsHan(lastUser) || containsHan(lastAssistant) {
		return "可以"
	}
	return "go ahead"
}

func containsSuggestionMode(text string) bool {
	clean := stripSystemRemindersForMode(text)
	return strings.Contains(strings.ToLower(clean), "suggestion mode")
}

// lastMessageText walks the history backwards and returns the stripped text of
// the newest message whose role matches and whose text satisfies accept.
func lastMessageText(messages []prompt.Message, role string, accept func(string) bool) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if !strings.EqualFold(strings.TrimSpace(msg.Role), role) {
			continue
		}
		text := strings.TrimSpace(stripSystemRemindersForMode(msg.ExtractText()))
		if text != "" && accept(text) {
			return text
		}
	}
	return ""
}

func lastNonSuggestionUserText(messages []prompt.Message) string {
	return lastMessageText(messages, "user", func(text string) bool { return !containsSuggestionMode(text) })
}

func lastAssistantText(messages []prompt.Message) string {
	return lastMessageText(messages, "assistant", func(string) bool { return true })
}

func buildToolGateMessage(messages []prompt.Message) string {
	if lastUserIsToolResultFollowup(messages) {
		original := lastNonToolResultUserText(messages)
		if looksLikeOptimizationRequest(original) {
			return "Use the provided tool results to answer the user's project optimization request directly. Tool access is unavailable for this turn, and any request to read, inspect, search, or review more files will be ignored. Stay specific to the current project and available code context. Do NOT call tools, do not describe a plan, and do not say you will first analyze or review the codebase. Give the best concrete project-specific recommendations now."
		}
		return "Use the provided tool results to answer the user's follow-up directly. Tool access is unavailable for this turn, and any request to read, inspect, search, or review more files will be ignored. Stay specific to the current project and available code context. Do NOT call tools, do not describe a plan, and answer now based only on the provided results."
	}
	return "Answer directly without calling tools or performing any file operations."
}

func hasExplicitNextStepOffer(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return false
	}
	englishMarkers := []string{
		"if you want",
		"if you'd like",
		"if you need",
		"i can continue",
		"i can also",
		"i can help",
		"i can restart",
		"i can check",
		"i can review",
		"i can commit",
		"i can push",
	}
	for _, marker := range englishMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	chineseMarkers := []string{
		"如果你要",
		"如果需要",
		"如果你愿意",
		"要的话",
		"需要的话",
		"我可以继续",
		"我可以直接",
		"我可以帮你",
		"我下一步可以",
	}
	return slices.ContainsFunc(chineseMarkers, func(marker string) bool { return strings.Contains(text, marker) })
}

func containsHan(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func containsNormalizedRequestText(text string, markers ...string) bool {
	lower := strings.ToLower(strings.TrimSpace(stripSystemRemindersForMode(text)))
	return slices.ContainsFunc(markers, func(marker string) bool { return strings.Contains(lower, marker) })
}

func looksLikeOptimizationRequest(text string) bool {
	return containsNormalizedRequestText(text,
		"怎么优化", "如何优化", "优化建议", "性能怎么优化", "重构建议", "改进建议",
		"帮我优化", "优化这个项目", "项目优化", "优化下这个项目", "帮我改进这个项目",
		"优化这个方案", "帮我优化这个方案", "优化这个设计", "帮我优化这个设计", "优化这个实现", "帮我优化这个实现",
		"how to optimize", "optimization advice", "performance optimization", "refactor suggestions", "improvement suggestions",
		"optimize this plan", "optimize this design", "optimize this implementation",
	)
}

func isTopicClassifierRequest(req ClaudeRequest) bool {
	for _, item := range req.System {
		if strings.ToLower(strings.TrimSpace(item.Type)) != "text" {
			continue
		}
		lower := strings.ToLower(stripSystemRemindersForMode(item.Text))
		if strings.Contains(lower, "new conversation topic") &&
			strings.Contains(lower, "isnewtopic") &&
			strings.Contains(lower, "json object") &&
			strings.Contains(lower, "title") {
			return true
		}
	}
	return false
}

func isTitleGenerationRequest(req ClaudeRequest) bool {
	hasTitleInstruction := false
	hasJSONInstruction := false

	for _, item := range req.System {
		if strings.ToLower(strings.TrimSpace(item.Type)) != "text" {
			continue
		}
		lower := strings.ToLower(stripSystemRemindersForMode(item.Text))
		if strings.Contains(lower, "generate a concise, sentence-case title") ||
			(strings.Contains(lower, "sentence-case title") && strings.Contains(lower, "coding session")) {
			hasTitleInstruction = true
		}
		if strings.Contains(lower, "return json with a single \"title\" field") ||
			(strings.Contains(lower, "return json") && strings.Contains(lower, "single") && strings.Contains(lower, "\"title\"")) {
			hasJSONInstruction = true
		}
	}

	return hasTitleInstruction && hasJSONInstruction
}

func classifyTopicRequest(req ClaudeRequest) (bool, string) {
	userTexts := extractUserTexts(req.Messages)
	if len(userTexts) == 0 {
		return false, ""
	}

	latest := strings.TrimSpace(userTexts[len(userTexts)-1])
	if latest == "" {
		return false, ""
	}

	prev := ""
	if len(userTexts) >= 2 {
		prev = strings.TrimSpace(userTexts[len(userTexts)-2])
	}

	if prev == "" {
		return true, generateTopicTitle(latest)
	}

	if isGreetingText(latest) {
		return false, ""
	}

	latestNorm := normalizeTopicText(latest)
	prevNorm := normalizeTopicText(prev)
	if latestNorm == "" || prevNorm == "" {
		return latest != prev, generateTopicTitle(latest)
	}
	if latestNorm == prevNorm || strings.Contains(latestNorm, prevNorm) || strings.Contains(prevNorm, latestNorm) {
		return false, ""
	}
	return true, generateTopicTitle(latest)
}

func extractUserTexts(messages []prompt.Message) []string {
	texts := make([]string, 0, len(messages))
	for _, msg := range messages {
		if strings.ToLower(strings.TrimSpace(msg.Role)) != "user" {
			continue
		}
		text := strings.TrimSpace(stripSystemRemindersForMode(msg.ExtractText()))
		if text != "" {
			texts = append(texts, text)
		}
	}
	return texts
}

func isGreetingText(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	switch lower {
	case "hi", "hello", "hey", "你好", "您好", "嗨", "在吗":
		return true
	default:
		return false
	}
}

func normalizeTopicText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func generateTopicTitle(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "New Topic"
	}
	words := strings.Fields(trimmed)
	if len(words) >= 2 {
		if len(words) > 3 {
			words = words[:3]
		}
		return strings.Join(words, " ")
	}
	runes := []rune(trimmed)
	if len(runes) > 10 {
		runes = runes[:10]
	}
	return strings.TrimSpace(string(runes))
}

// stripSystemRemindersForMode removes <system-reminder>...</system-reminder>, so a
// plan/suggestion mode is not misdetected.
// It locates the closing tag with LastIndex, which handles nested literal tags.
func stripSystemRemindersForMode(text string) string {
	text = stripModeTaggedBlock(text, "system-reminder", true)
	for _, tag := range []string{
		"local-command-caveat",
		"command-name",
		"command-message",
		"command-args",
		"local-command-stdout",
		"local-command-stderr",
		"local-command-exit-code",
	} {
		text = stripModeTaggedBlock(text, tag, false)
	}
	return text
}

func stripModeTaggedBlock(text string, tag string, nested bool) string {
	startTag := "<" + tag + ">"
	endTag := "</" + tag + ">"
	if !strings.Contains(text, startTag) {
		return text
	}
	var sb strings.Builder
	sb.Grow(len(text))
	i := 0
	for i < len(text) {
		start := strings.Index(text[i:], startTag)
		if start == -1 {
			sb.WriteString(text[i:])
			break
		}
		sb.WriteString(text[i : i+start])
		blockStart := i + start
		endStart := blockStart + len(startTag)
		end := strings.Index(text[endStart:], endTag)
		if nested {
			end = strings.LastIndex(text[endStart:], endTag)
		}
		if end == -1 {
			sb.WriteString(text[blockStart:])
			break
		}
		i = endStart + end + len(endTag)
	}
	return sb.String()
}
