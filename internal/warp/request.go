package warp

import (
	"encoding/json"
	"reflect"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/structpb"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

const (
	userTextLimit   = 4096
	systemTextLimit = 8192
	// historyTurns bounds how much prior conversation may be replayed. It is a
	// deliberate cost ceiling, not a protocol limit: Warp accepts a full task
	// context, but replaying an unbounded transcript is what made earlier
	// versions expensive.
	historyTurns    = 8
	transcriptLimit = 64 << 10
	toolCountLimit  = 64
	// toolRoundLimit caps agent loops per conversation. Every tool round costs a
	// paid inference call, so an unbounded loop is exactly the cost failure this
	// channel must not repeat.
	toolRoundLimit     = 6
	toolSchemaLimit    = 64 << 10
	toolDescriptionCap = 4 << 10
)

type mcpTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// preparedRequest is everything the wire builder needs, produced before any
// credential refresh or paid call.
type preparedRequest struct {
	Query         string
	Tools         []mcpTool
	ParallelTools bool
	ContextWindow int
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
// one optional instruction message, a bounded recent transcript, and a final user
// turn. Tools are declared to upstream as client MCP tools so the client, not the
// gateway, executes them.
func validateRequest(req upstream.UpstreamRequest) (preparedRequest, error) {
	reject := func() (preparedRequest, error) {
		return preparedRequest{}, apperrors.New("invalid_request_error", "Warp supports a bounded text conversation (max 8 prior turns, 64 KiB) with one user text message (1-4096 bytes) and optional client tools; no server-side tools, images, or generation controls", 400)
	}
	// Generation controls are accepted for wire compatibility with Anthropic and
	// OpenAI clients but are not forwarded: the Warp agent endpoint exposes no
	// equivalent, so the gateway's own byte ceilings bound the answer.
	if len(req.Messages) < 1 || len(req.Messages) > 2*historyTurns+2 || req.ReasoningEffort != "" || req.ResponseFormat != nil || req.ResponseText != nil || req.Include != nil || req.PromptCacheKey != "" || len(req.ResponsesTools) != 0 {
		return reject()
	}
	tools, ok := convertTools(req.Tools)
	if !ok {
		return reject()
	}
	choice, disableParallel, ok := parseToolChoice(req.ToolChoice)
	if !ok {
		return reject()
	}
	if choice == "none" {
		tools = nil
	}
	parallel := req.ParallelToolCalls != nil && *req.ParallelToolCalls && !disableParallel && len(tools) > 0
	messages := req.Messages
	if messages[len(messages)-1].Role != "user" {
		return reject()
	}
	var system string
	if len(req.System) > 0 {
		parts := make([]string, 0, len(req.System))
		for _, item := range req.System {
			if strings.TrimSpace(item.Text) == "" || !utf8.ValidString(item.Text) || !reflect.DeepEqual(item, prompt.SystemItem{Type: item.Type, Text: item.Text}) || (item.Type != "" && item.Type != "text") {
				return reject()
			}
			parts = append(parts, item.Text)
		}
		system = strings.Join(parts, "\n")
		if len(system) > systemTextLimit {
			return reject()
		}
	}
	start := 0
	if messages[0].Role == "system" {
		systemText, ok := messageText(messages[0], systemTextLimit)
		if !ok || strings.TrimSpace(systemText) == "" {
			return reject()
		}
		if system != "" {
			return reject()
		}
		system, start = systemText, 1
		if len(messages) == 1 {
			return reject()
		}
	}
	// The final user turn may carry tool results produced by the client. Those
	// travel as structured inputs; every earlier turn is rendered as transcript.
	prior, currentText, results, ok := splitCurrentTurn(messages, start)
	if !ok {
		return reject()
	}
	if len(prior) == 0 && len(results) > 0 {
		return reject()
	}
	if len(currentText) > userTextLimit || !utf8.ValidString(currentText) || (strings.TrimSpace(currentText) == "" && len(results) == 0) {
		return reject()
	}
	for i, message := range prior {
		role := message.Role
		if role != "user" && role != "assistant" {
			return reject()
		}
		if i == 0 && role != "user" {
			return reject()
		}
		if i > 0 && prior[i-1].Role == role {
			return reject()
		}
		if _, ok := renderMessage(message); !ok {
			return reject()
		}
	}
	if len(prior)%2 != 0 || len(prior) > 2*historyTurns {
		return reject()
	}
	if toolRounds(prior) > toolRoundLimit {
		return preparedRequest{}, apperrors.New("invalid_request_error", "Warp tool loop limit reached for this conversation; start a new conversation", 400)
	}
	// Resolve each current tool result to the call it answers. Anything the
	// client is not able to type back is rendered as text instead.
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
	if len(history) == 1 && system == "" && start == 0 && history[0].Role == "user" && len(history[0].Content.Blocks) == 0 && len(preparedResults) == 0 {
		text := history[0].Content.Text
		if strings.TrimSpace(text) == "" || len(text) > userTextLimit || !validModel(req.Model) {
			return reject()
		}
		return preparedRequest{Query: text, CurrentText: text, Tools: tools, ParallelTools: parallel}, nil
	}
	// Reserve the exact stateless fallback, including labels and separators, even
	// when a binding will send these results as structured inputs instead.
	fallback := appendToolResultsText("", preparedResults)
	reserved := len(fallback)
	if reserved > 0 {
		reserved += len("\n\n")
	}
	transcript, ok := renderTranscriptWithin(system, history, transcriptLimit-reserved)
	if !ok || !validModel(req.Model) {
		return reject()
	}
	if transcript == "" && len(preparedResults) == 0 {
		return reject()
	}
	return preparedRequest{Query: transcript, CurrentText: currentText, Tools: tools, ParallelTools: parallel, ToolResults: preparedResults}, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
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

// appendToolResultsText renders unbound tool results into the transcript so a
// stateless request still carries the client's findings.
func appendToolResultsText(query string, results []preparedToolResult) string {
	if len(results) == 0 {
		return query
	}
	parts := make([]string, 0, len(results))
	for _, result := range results {
		parts = append(parts, "Tool result "+toolLabel(result.ID, result.Name)+": "+result.Content)
	}
	if strings.TrimSpace(query) == "" {
		return strings.Join(parts, "\n\n")
	}
	return query + "\n\n" + strings.Join(parts, "\n\n")
}

// toolRounds counts how many tool calls the transcript already contains, which
// is the number of paid turns this conversation has consumed.
func toolRounds(messages []prompt.Message) int {
	rounds := 0
	for _, m := range messages {
		for _, block := range m.Content.Blocks {
			if block.Type == "tool_use" {
				rounds++
			}
		}
	}
	return rounds
}

// renderTranscript flattens the bounded conversation into one query string. Tool
// calls and their results are rendered as text so a stateless turn needs no
// server-side task binding.
func renderTranscript(system string, messages []prompt.Message) (string, bool) {
	return renderTranscriptWithin(system, messages, transcriptLimit)
}

func renderTranscriptWithin(system string, messages []prompt.Message, limit int) (string, bool) {
	if limit < 0 {
		return "", false
	}
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		rendered, ok := renderMessage(m)
		if !ok || rendered == "" {
			return "", false
		}
		parts = append(parts, rendered)
	}
	// The instruction prefix is pinned. Only complete oldest user/assistant
	// pairs may be omitted; the current user input (or latest tool-call pair
	// when results are structured separately) is always retained.
	for dropped := 0; ; dropped += 2 {
		all := make([]string, 0, len(parts)+2)
		if system != "" {
			all = append(all, "System instructions:\n"+system)
		}
		if dropped > 0 {
			all = append(all, "[earlier history omitted]")
		}
		all = append(all, parts[dropped:]...)
		transcript := strings.Join(all, "\n\n")
		if len(transcript) <= limit {
			return transcript, true
		}
		// Keep the latest complete history pair, unless a current user message
		// exists and is the only context that can fit the hard ceiling.
		if len(parts)-dropped <= 2 || messages[dropped].Role != "user" || messages[dropped+1].Role != "assistant" {
			return "", false
		}
	}
}

func renderMessage(m prompt.Message) (string, bool) {
	if (m.Role != "user" && m.Role != "assistant") || m.ReasoningContent != "" || len(m.ReasoningItem) != 0 || (m.Content.Blocks != nil && m.Content.Text != "") {
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
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				lines = append(lines, prefix+": "+block.Text)
			}
		case "tool_use":
			args, err := json.Marshal(block.Input)
			if err != nil || len(args) > transcriptLimit {
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
	case "text":
		expected = prompt.ContentBlock{Type: "text", Text: block.Text}
		if !utf8.ValidString(block.Text) {
			return false
		}
	case "tool_use":
		input, ok := block.Input.(map[string]any)
		if role != "assistant" || block.ID == "" || !utf8.ValidString(block.ID) || !validToolName(block.Name) || !ok || input == nil {
			return false
		}
		expected = prompt.ContentBlock{Type: "tool_use", ID: block.ID, Name: block.Name, Input: block.Input}
	case "tool_result":
		if role != "user" || block.ToolUseID == "" || !utf8.ValidString(block.ToolUseID) {
			return false
		}
		if _, ok := toolResultText(block.Content); !ok {
			return false
		}
		expected = prompt.ContentBlock{Type: "tool_result", ToolUseID: block.ToolUseID, Content: block.Content, IsError: block.IsError}
	default:
		return false
	}
	return reflect.DeepEqual(block, expected)
}

// toolResultText accepts only text payloads, never image/file or opaque objects.
func toolResultText(content any) (string, bool) {
	switch typed := content.(type) {
	case string:
		return typed, utf8.ValidString(typed) && len(typed) <= transcriptLimit
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
		return out, len(out) <= transcriptLimit
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

// convertTools accepts exactly Anthropic client tools or nested OpenAI function
// tools. Schema/name/description values are forwarded unchanged, never repaired.
func convertTools(raw []any) ([]mcpTool, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	if len(raw) > toolCountLimit {
		return nil, false
	}
	tools := make([]mcpTool, 0, len(raw))
	total := 0
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok || entry == nil {
			return nil, false
		}
		fn, schemaKey := entry, "input_schema"
		if _, nested := entry["function"]; nested {
			if !toolKeys(entry, "type", "function") || entry["type"] != "function" {
				return nil, false
			}
			fn, ok = entry["function"].(map[string]any)
			if !ok || fn == nil || !toolKeys(fn, "name", "description", "parameters", "strict") {
				return nil, false
			}
			// Warp's MCP dialect cannot promise strict output validation. An
			// explicit false is equivalent to its normal non-strict behavior.
			if strict, exists := fn["strict"]; exists {
				value, valid := strict.(bool)
				if !valid || value {
					return nil, false
				}
			}
			schemaKey = "parameters"
		} else if !toolKeys(entry, "name", "description", "input_schema") {
			return nil, false
		}
		name, ok := fn["name"].(string)
		if !ok || !validToolName(name) {
			return nil, false
		}
		for _, existing := range tools {
			if strings.EqualFold(existing.Name, name) {
				return nil, false
			}
		}
		tool := mcpTool{Name: name}
		if description, exists := fn["description"]; exists {
			desc, valid := description.(string)
			if !valid || len(desc) > toolDescriptionCap || !utf8.ValidString(desc) {
				return nil, false
			}
			tool.Description = desc
		}
		params, ok := fn[schemaKey].(map[string]any)
		if !ok || params == nil {
			return nil, false
		}
		encoded, err := json.Marshal(params)
		if err != nil || len(encoded) > toolSchemaLimit {
			return nil, false
		}
		// Validate the actual wire representation before a paid operation;
		// json.Marshal alone also accepts values structpb cannot encode.
		if _, err := structpb.NewStruct(params); err != nil {
			return nil, false
		}
		total += len(encoded)
		if total > toolSchemaLimit {
			return nil, false
		}
		tool.Schema = params
		tools = append(tools, tool)
	}
	return tools, true
}

// messageText extracts plain text from one message. Content blocks must be pure
// text: no images, files, or reasoning payloads may enter a paid request.
func messageText(m prompt.Message, limit int) (string, bool) {
	if m.ReasoningContent != "" || len(m.ReasoningItem) != 0 {
		return "", false
	}
	text := m.Content.Text
	if m.Content.Blocks != nil {
		if text != "" {
			return "", false
		}
		for _, block := range m.Content.Blocks {
			if block.Type != "text" || !reflect.DeepEqual(block, prompt.ContentBlock{Type: "text", Text: block.Text}) {
				return "", false
			}
			text += block.Text
			if len(text) > limit {
				return "", false
			}
		}
	}
	if len(text) > limit || !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}
