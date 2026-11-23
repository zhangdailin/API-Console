package warp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/structpb"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

type mcpTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// preparedRequest is everything the wire builder needs, produced before any
// credential refresh or paid call.
type preparedRequest struct {
	// Resume uses the official recovery input and never resends tool results.
	Resume        bool
	Query         string
	Tools         []mcpTool
	ParallelTools bool
	ContextWindow int
	Images        []warpImage
	// HistoryImages counts the leading attachments replayed on a stateless turn.
	HistoryImages int
	// CurrentText is the new instruction in this turn, without replayed history.
	// It is what a resumed conversation sends; Query is the stateless transcript.
	CurrentText string
	// ConversationID resumes an upstream conversation when a binding was found.
	ConversationID  string
	RootTaskContext []byte
	ToolRounds      int
	// ToolResults closes the client-executed tool loop for the current turn.
	ToolResults []preparedToolResult
}

// preparedToolResult is one tool result the client produced for a call this
// conversation already saw. It is keyed by the upstream tool call id.
type preparedToolResult struct {
	ID      string
	Name    string
	Content string
	IsError bool
}

// validateRequest runs before token renewal or any upstream operation. It accepts
// plain-text instructions, user images, the complete transcript, and a final user
// turn. Tools are declared to upstream as client MCP tools so the client, not the
// gateway, executes them.
func validateRequest(req upstream.UpstreamRequest) (preparedRequest, error) {
	reject := func() (preparedRequest, error) {
		return preparedRequest{}, apperrors.NewLocalRequestError("warp_request_shape")
	}
	// Generation controls are accepted for wire compatibility with Anthropic and
	// OpenAI clients but are not forwarded: the Warp agent endpoint exposes no
	// equivalent. The gateway does not impose a token-saving answer ceiling.
	if len(req.Messages) < 1 || req.ResponseFormat != nil || req.ResponseText != nil || req.Include != nil || req.PromptCacheKey != "" || len(req.ResponsesTools) != 0 {
		return reject()
	}
	if err := validateThinkingHistory(req.Messages); err != nil {
		return preparedRequest{}, err
	}
	tools, err := convertToolsWithError(req.Tools)
	if err != nil {
		return preparedRequest{}, err
	}
	choice, disableParallel, ok := parseToolChoice(req.ToolChoice)
	if !ok {
		return preparedRequest{}, apperrors.NewLocalRequestError("warp_tool_choice")
	}
	if choice == "none" {
		tools = nil
	}
	parallel := req.ParallelToolCalls != nil && *req.ParallelToolCalls && !disableParallel && len(tools) > 0
	// Anthropic's separate system blocks precede message-level instructions.
	// Remove instruction roles before validating the remaining dialog; some
	// clients append their instruction message after the current user turn.
	system, messages, err := normalizeInstructions(req.System, req.Messages)
	if err != nil {
		return preparedRequest{}, err
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
		return preparedRequest{}, apperrors.NewLocalRequestError("warp_dialog")
	}
	messages, images, historical, err := prepareImageContext(messages)
	if err != nil {
		return preparedRequest{}, err
	}
	prepared := preparedRequest{Tools: tools, ParallelTools: parallel, Images: images, HistoryImages: historical}
	// Validate the final original user message before joining context fragments.
	_, finalText, finalResults, ok := splitCurrentTurn(messages, 0)
	if !ok || (strings.TrimSpace(finalText) == "" && len(finalResults) == 0) {
		return preparedRequest{}, apperrors.NewLocalRequestError("warp_user_turn")
	}
	messages, ok = mergeUserContext(messages)
	if !ok {
		return reject()
	}
	// The final user turn may carry tool results produced by the client. Those
	// travel as structured inputs; every earlier turn is rendered as transcript.
	prior, currentText, results, ok := splitCurrentTurn(messages, 0)
	if !ok {
		return reject()
	}
	if len(prior) == 0 && len(results) > 0 {
		return reject()
	}
	if !utf8.ValidString(currentText) || (strings.TrimSpace(currentText) == "" && len(results) == 0) {
		return reject()
	}
	for i, message := range prior {
		role := message.Role
		if role != "user" && role != "assistant" {
			return preparedRequest{}, apperrors.NewLocalRequestError("warp_dialog")
		}
		if i == 0 && role != "user" {
			return preparedRequest{}, apperrors.NewLocalRequestError("warp_dialog")
		}
		if i > 0 && prior[i-1].Role == role {
			return preparedRequest{}, apperrors.NewLocalRequestError("warp_dialog")
		}
		if _, ok := renderMessage(message); !ok {
			return reject()
		}
	}
	if len(prior)%2 != 0 {
		return preparedRequest{}, apperrors.NewLocalRequestError("warp_dialog")
	}
	// Resolve each current tool result to the call it answers. Anything the
	// client is not able to type back is rendered as text instead.
	// A matching call requires its owned binding, never an unrelated paid
	// stateless replay. Historical calls do not impose a generation budget.
	uses := indexToolUses(prior)
	declared := make(map[string]bool, len(tools))
	for _, tool := range tools {
		declared[strings.ToLower(tool.Name)] = true
	}
	var preparedResults []preparedToolResult
	var untyped []string
	for _, result := range results {
		name := uses[result.ID]
		if name == "" || !declared[strings.ToLower(name)] {
			untyped = append(untyped, "Tool result "+toolLabel(result.ID, "")+": "+result.Content)
			continue
		}
		preparedResults = append(preparedResults, preparedToolResult{ID: result.ID, Name: name, Content: result.Content, IsError: result.IsError})
	}
	history := append([]prompt.Message(nil), prior...)
	currentParts := []string{}
	if currentText != "" {
		currentParts = append(currentParts, currentText)
	}
	currentParts = append(currentParts, untyped...)
	if len(currentParts) > 0 {
		history = append(history, prompt.Message{Role: "user", Content: prompt.MessageContent{Text: strings.Join(currentParts, "\n")}})
	}
	// A single plain user turn keeps the original, minimal wire shape.
	if len(history) == 1 && system == "" && history[0].Role == "user" && len(history[0].Content.Blocks) == 0 && len(preparedResults) == 0 {
		text := history[0].Content.Text
		if strings.TrimSpace(text) == "" || !validModel(req.Model) {
			return reject()
		}
		prepared.Query, prepared.CurrentText = text, text
		return prepared, nil
	}
	transcript, ok := renderTranscript(system, history)
	if !ok {
		return reject()
	}
	if !validModel(req.Model) {
		return reject()
	}
	if transcript == "" && len(preparedResults) == 0 {
		return reject()
	}
	continuationText := strings.Join(currentParts, "\n")
	if len(preparedResults) > 0 && system != "" {
		// A bound continuation omits the replayed dialog, not the caller's
		// instructions. Keep new/changed system text in the current query.
		prefix := "System instructions:\n" + system
		if continuationText != "" {
			prefix += "\n\nUser message:\n" + continuationText
		}
		continuationText = prefix
	}
	prepared.Query, prepared.CurrentText, prepared.ToolResults = transcript, continuationText, preparedResults
	return prepared, nil
}

// splitCurrentTurn separates the final user turn into plain text and the tool
// results it returns. Tool results never count as answer text.
func splitCurrentTurn(messages []prompt.Message, start int) ([]prompt.Message, string, []promptToolResult, bool) {
	last := messages[len(messages)-1]
	if last.ReasoningContent != "" || len(last.ReasoningItem) != 0 || (last.Content.Blocks != nil && last.Content.Text != "") {
		return nil, "", nil, false
	}
	if last.Content.Blocks == nil {
		if !utf8.ValidString(last.Content.Text) {
			return nil, "", nil, false
		}
		return messages[start : len(messages)-1], last.Content.Text, nil, true
	}
	var text strings.Builder
	var results []promptToolResult
	for _, block := range last.Content.Blocks {
		if !validMessageBlock(block, "user") {
			return nil, "", nil, false
		}
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_result":
			if block.ToolUseID == "" {
				return nil, "", nil, false
			}
			content, ok := toolResultText(block.Content)
			if !ok {
				return nil, "", nil, false
			}
			results = append(results, promptToolResult{ID: block.ToolUseID, Content: content, IsError: block.IsError})
		default:
			return nil, "", nil, false
		}
	}
	return messages[start : len(messages)-1], text.String(), results, true
}

type promptToolResult struct {
	ID      string
	Content string
	IsError bool
}

func indexToolUses(messages []prompt.Message) map[string]string {
	uses := map[string]string{}
	for _, message := range messages {
		for _, block := range message.Content.Blocks {
			if block.Type == "tool_use" && block.ID != "" && block.Name != "" {
				uses[block.ID] = block.Name
			}
		}
	}
	return uses
}

// renderTranscript flattens the complete conversation into one query string. Tool
// calls and their results are rendered as text so a stateless turn needs no
// server-side task binding.
func renderTranscript(system string, messages []prompt.Message) (string, bool) {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		rendered, ok := renderMessage(m)
		if !ok || rendered == "" {
			return "", false
		}
		parts = append(parts, rendered)
	}
	// Preserve all supplied instructions and dialog without local trimming.
	prefix := ""
	if system != "" {
		prefix = "System instructions:\n" + system + "\n\n"
	}
	return prefix + strings.Join(parts, "\n\n"), true
}

func renderMessage(m prompt.Message) (string, bool) {
	// Chat reasoning_content is metadata, just like an assistant thinking
	// block. Keep it outside the query even when the visible answer is replayed.
	if (m.Role != "user" && m.Role != "assistant") || (m.Role != "assistant" && m.ReasoningContent != "") ||
		len(m.ReasoningItem) != 0 || (m.Content.Blocks != nil && m.Content.Text != "") {
		return "", false
	}
	prefix := "User"
	if m.Role == "assistant" {
		prefix = "Assistant"
	}
	if m.Content.Blocks == nil {
		if !utf8.ValidString(m.Content.Text) || strings.TrimSpace(m.Content.Text) == "" {
			return "", false
		}
		return prefix + ": " + m.Content.Text, true
	}
	lines := make([]string, 0, len(m.Content.Blocks))
	for _, block := range m.Content.Blocks {
		if !validMessageBlock(block, m.Role) {
			return "", false
		}
		switch block.Type {
		case "thinking":
			// Historical private reasoning is not a user instruction, tool
			// parameter, visible answer, or transferable provider signature.
			continue
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				lines = append(lines, prefix+": "+block.Text)
			}
		case "tool_use":
			args, err := json.Marshal(block.Input)
			if err != nil {
				return "", false
			}
			lines = append(lines, "Assistant tool call "+toolLabel(block.ID, block.Name)+": "+string(args))
		case "tool_result":
			content, ok := toolResultText(block.Content)
			if !ok {
				return "", false
			}
			lines = append(lines, "Tool result "+toolLabel(block.ToolUseID, "")+": "+content)
		default:
			return "", false
		}
	}
	if len(lines) == 0 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

func toolLabel(id, name string) string {
	switch {
	case id != "" && name != "":
		return "(id=" + id + ", name=" + name + ")"
	case id != "":
		return "(id=" + id + ")"
	case name != "":
		return "(name=" + name + ")"
	default:
		return ""
	}
}

// validMessageBlock rejects fields belonging to other content variants even
// when normalized callers bypass the raw JSON allowlist.
func validMessageBlock(block prompt.ContentBlock, role string) bool {
	var expected prompt.ContentBlock
	switch block.Type {
	case "thinking":
		if !validThinkingHistory(block, role) {
			return false
		}
		expected = prompt.ContentBlock{Type: "thinking", Thinking: block.Thinking, Signature: block.Signature}
	case "text":
		expected = prompt.ContentBlock{Type: "text", Text: block.Text, CacheControl: block.CacheControl}
		if !validCacheControl(block.CacheControl) || !utf8.ValidString(block.Text) {
			return false
		}
	case "tool_use":
		input, ok := block.Input.(map[string]any)
		if role != "assistant" || block.ID == "" || !utf8.ValidString(block.ID) || !validToolName(block.Name) || !ok || input == nil || !validCacheControl(block.CacheControl) {
			return false
		}
		expected = prompt.ContentBlock{Type: "tool_use", ID: block.ID, Name: block.Name, Input: block.Input, CacheControl: block.CacheControl}
	case "tool_result":
		if role != "user" || block.ToolUseID == "" || !utf8.ValidString(block.ToolUseID) || !validCacheControl(block.CacheControl) {
			return false
		}
		if _, ok := toolResultText(block.Content); !ok {
			return false
		}
		expected = prompt.ContentBlock{Type: "tool_result", ToolUseID: block.ToolUseID, Content: block.Content, IsError: block.IsError, CacheControl: block.CacheControl}
	default:
		return false
	}
	return reflect.DeepEqual(block, expected)
}

// toolResultText accepts only text payloads, never image/file or opaque objects.
func toolResultText(content any) (string, bool) {
	switch typed := content.(type) {
	case string:
		return typed, utf8.ValidString(typed)
	case []any:
		if typed == nil {
			return "", false
		}
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok || !toolKeys(block, "type", "text") || block["type"] != "text" {
				return "", false
			}
			text, ok := block["text"].(string)
			if !ok || !utf8.ValidString(text) {
				return "", false
			}
			parts = append(parts, text)
		}
		out := strings.Join(parts, "\n")
		return out, true
	default:
		return "", false
	}
}

func parseToolChoice(raw any) (choice string, disableParallel bool, ok bool) {
	if raw == nil {
		return "auto", false, true
	}
	switch value := raw.(type) {
	case string:
		choice = value
	case map[string]any:
		if value == nil || !toolKeys(value, "type", "disable_parallel_tool_use") {
			return "", false, false
		}
		choice, ok = value["type"].(string)
		if !ok {
			return "", false, false
		}
		if rawDisable, exists := value["disable_parallel_tool_use"]; exists {
			disableParallel, ok = rawDisable.(bool)
			if !ok {
				return "", false, false
			}
		}
	default:
		return "", false, false
	}
	return choice, disableParallel, choice == "auto" || choice == "none"
}

func toolKeys(object map[string]any, allowed ...string) bool {
	for key := range object {
		found := false
		for _, candidate := range allowed {
			if key == candidate {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validToolName(name string) bool {
	return strings.TrimSpace(name) != "" && len(name) <= 128 && utf8.ValidString(name) && !strings.ContainsAny(name, "\r\n\x00")
}

// requestFieldError reports only gateway-owned reasons and paths, never input
// values. Counts and byte measurements are safe to expose to clients.
func requestFieldError(code, path string) error {
	return apperrors.New("invalid_request_error", "Warp "+code+" at "+path, 400)
}

func convertToolsWithError(raw []any) ([]mcpTool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	tools := make([]mcpTool, 0, len(raw))
	for i, item := range raw {
		path := fmt.Sprintf("tools[%d]", i)
		entry, ok := item.(map[string]any)
		if !ok || entry == nil {
			return nil, requestFieldError("invalid_tool", path)
		}
		fn, schemaKey, fnPath := entry, "input_schema", path
		if _, nested := entry["function"]; nested {
			if !toolKeys(entry, "type", "function") || entry["type"] != "function" {
				return nil, requestFieldError("invalid_tool", path)
			}
			fnPath += ".function"
			fn, ok = entry["function"].(map[string]any)
			if !ok || fn == nil || !toolKeys(fn, "name", "description", "parameters", "strict") {
				return nil, requestFieldError("invalid_tool", fnPath)
			}
			// Warp cannot promise strict output validation. Explicit false is
			// equivalent to its normal non-strict behavior.
			if strict, exists := fn["strict"]; exists {
				value, valid := strict.(bool)
				if !valid || value {
					return nil, requestFieldError("unsupported_tool_strict", fnPath+".strict")
				}
			}
			schemaKey = "parameters"
		} else {
			if !toolKeys(entry, "name", "description", "input_schema", "eager_input_streaming") {
				return nil, requestFieldError("invalid_tool", path)
			}
			if eager, exists := entry["eager_input_streaming"]; exists {
				if _, valid := eager.(bool); !valid {
					return nil, requestFieldError("invalid_eager_input_streaming", path+".eager_input_streaming")
				}
			}
		}
		name, ok := fn["name"].(string)
		if !ok || !validToolName(name) {
			return nil, requestFieldError("invalid_tool_name", fnPath+".name")
		}
		for _, existing := range tools {
			if strings.EqualFold(existing.Name, name) {
				return nil, requestFieldError("duplicate_tool_name", fnPath+".name")
			}
		}
		tool := mcpTool{Name: name}
		if description, exists := fn["description"]; exists {
			desc, valid := description.(string)
			if !valid || !utf8.ValidString(desc) {
				return nil, requestFieldError("invalid_tool_description", fnPath+".description")
			}
			tool.Description = desc
		}
		params, ok := fn[schemaKey].(map[string]any)
		if !ok || params == nil {
			return nil, requestFieldError("invalid_tool_schema", fnPath+"."+schemaKey)
		}
		_, err := json.Marshal(params)
		if err != nil {
			return nil, requestFieldError("invalid_tool_schema", fnPath+"."+schemaKey)
		}
		// Validate the actual wire representation before a paid operation;
		// json.Marshal alone also accepts values structpb cannot encode.
		if _, err := structpb.NewStruct(params); err != nil {
			return nil, requestFieldError("invalid_tool_schema", fnPath+"."+schemaKey)
		}
		// Preserve every definition without size/count caps or schema rewriting.
		tool.Schema = params
		tools = append(tools, tool)
	}
	return tools, nil
}

// normalizeInstructions preserves all instruction text and its source order.
// Cache markers are validated hints, not instructions supported by Warp.
func normalizeInstructions(items []prompt.SystemItem, messages []prompt.Message) (string, []prompt.Message, error) {
	parts := make([]string, 0, len(items))
	for i, item := range items {
		path := fmt.Sprintf("system[%d]", i)
		if (item.Type != "" && item.Type != "text") || strings.TrimSpace(item.Text) == "" || !utf8.ValidString(item.Text) {
			return "", nil, requestFieldError("invalid_system_text", path)
		}
		if !validCacheControl(item.CacheControl) {
			return "", nil, requestFieldError("invalid_cache_control", path+".cache_control")
		}
		parts = append(parts, item.Text)
	}
	dialog := make([]prompt.Message, 0, len(messages))
	for i, message := range messages {
		if message.Role != "system" && message.Role != "developer" {
			dialog = append(dialog, message)
			continue
		}
		text, ok := messageText(message)
		if !ok || strings.TrimSpace(text) == "" {
			return "", nil, requestFieldError("invalid_system_text", fmt.Sprintf("messages[%d].content", i))
		}
		parts = append(parts, text)
	}
	system := strings.Join(parts, "\n")
	return system, dialog, nil
}

// TTL and other raw cache-control fields are validated before decoding; the
// shared normalized type retains only Type. Warp ignores the validated hint.
func validCacheControl(cache *prompt.CacheControl) bool {
	return cache == nil || cache.Type == "ephemeral"
}

// messageText extracts plain text from one message. Content blocks must be pure
// text: no images, files, or reasoning payloads may enter a paid request.
func messageText(m prompt.Message) (string, bool) {
	if m.ReasoningContent != "" || len(m.ReasoningItem) != 0 {
		return "", false
	}
	text := m.Content.Text
	if m.Content.Blocks != nil {
		if text != "" {
			return "", false
		}
		parts := make([]string, 0, len(m.Content.Blocks))
		for _, block := range m.Content.Blocks {
			if block.Type != "text" || !validMessageBlock(block, "system") {
				return "", false
			}
			parts = append(parts, block.Text)
		}
		// Preserve adjacent block concatenation used by existing clients; only
		// distinct instruction messages/items receive newline separators.
		text = strings.Join(parts, "")
	}
	if !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}
