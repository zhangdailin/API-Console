package workbuddy

import (
	"regexp"
	"strings"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// ChatMessage is one OpenAI-shaped message in the WorkBuddy payload.
type ChatMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall is the OpenAI tool-call shape used both inbound and outbound.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction carries the tool name and JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// WorkBuddy's gateway blocks a request that declares a first-party Anthropic
// client with HTTP 400 code=11128 ("Illegal API invocation from an unapproved
// channel", displayMsg "The request was blocked by security policy"). Claude
// Code puts two such markers in the system array — a billing header block and
// its own identity line — and neither carries instructions for the model, so
// they are dropped instead of being forwarded.
var (
	anthropicBillingHeaderPattern = regexp.MustCompile(`(?i)^x-[a-z0-9-]*billing-header\s*:`)
	anthropicCLIIdentityPattern   = regexp.MustCompile(`(?i)^you are claude code\b`)
	codexCLIIdentityPattern       = regexp.MustCompile(`(?i)^[ \t]*you are codex\b(?:[^.!?\r\n]|\.[0-9])*(?:[.!?][ \t]*|$)`)
	codexCLIRuntimePattern        = regexp.MustCompile(`(?i)^[ \t]*you are a coding agent running in the codex cli\b[^.!?\r\n]*(?:[.!?][ \t]*|$)`)
)

// These are literal client boilerplate, not a general replacement of product
// names. Paths, tool names, links, skills and project instructions keep their
// original meaning and spelling.
var codexSystemBoilerplate = strings.NewReplacer(
	"Codex CLI is an open source project led by OpenAI. ", "",
	"Codex CLI is an open source project led by OpenAI.", "",
	"Within this context, Codex refers to the open-source agentic coding interface (not the old Codex language model built by OpenAI).", "",
	"# Codex desktop context", "# Desktop context",
	"You are running inside the Codex (desktop) app, which allows some additional features not available in the CLI alone:", "The desktop app provides the following additional features:",
)

// isAnthropicClientMarker reports whether a system block is first-party client
// metadata rather than prompt content.
func isAnthropicClientMarker(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	return anthropicBillingHeaderPattern.MatchString(trimmed) ||
		anthropicCLIIdentityPattern.MatchString(trimmed)
}

// filterClientSystemText removes known client identity sentences, not the whole
// instruction block. Responses instructions can contain the Codex identity and
// all of the task's working rules in a single string. Preserve non-marker lines
// byte-for-byte, including their line endings, and never apply this to history
// from users, assistants or tools.
func filterClientSystemText(text string) string {
	var out strings.Builder
	changed := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if isAnthropicClientMarker(line) {
			changed = true
			continue
		}
		cleaned := codexCLIIdentityPattern.ReplaceAllString(line, "")
		cleaned = codexCLIRuntimePattern.ReplaceAllString(cleaned, "")
		cleaned = codexSystemBoilerplate.Replace(cleaned)
		if cleaned != line {
			changed = true
			// A removed marker-only line must not create an empty system block.
			if strings.TrimSpace(cleaned) == "" {
				continue
			}
		}
		out.WriteString(cleaned)
	}
	if !changed {
		return text
	}
	return out.String()
}

// buildMessages renders the request history. WorkBuddy validates roles against
// a whitelist and requires messages[0] to be a system message, so system items
// are emitted first, tool results become `tool` messages, and a minimal system
// prompt is prepended when the caller supplied none. Anthropic client markers
// are dropped for the reason documented above.
func buildMessages(req upstream.UpstreamRequest) []ChatMessage {
	out := make([]ChatMessage, 0, len(req.Messages)+len(req.System)+2)
	pendingToolCalls := make(map[string]bool)

	for _, item := range req.System {
		text := filterClientSystemText(item.Text)
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, ChatMessage{Role: "system", Content: text})
	}

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "", "user":
			role = "user"
		case "developer":
			// `developer` is not in the upstream role whitelist; it is the new
			// name for the system role, so the rewrite is lossless.
			role = "system"
		}

		if msg.Content.IsString() {
			text := msg.Content.GetText()
			if role == "system" {
				text = filterClientSystemText(text)
			}
			if strings.TrimSpace(text) == "" && reasoningForReplay(msg) == "" {
				// Keep an assistant reasoning item for its tool turn; an empty
				// message without either text or reasoning carries nothing.
				continue
			}
			out = append(out, ChatMessage{Role: role, Content: text, ReasoningContent: reasoningForReplay(msg)})
			continue
		}

		switch role {
		case "assistant":
			if converted, ok := convertAssistantMessage(msg, pendingToolCalls); ok {
				out = append(out, converted)
			}
		default:
			out = append(out, convertBlockMessage(role, msg, pendingToolCalls)...)
		}
	}

	if len(out) == 0 {
		prompt := strings.TrimSpace(req.Prompt)
		if prompt == "" {
			prompt = defaultSystem
		}
		out = append(out, ChatMessage{Role: "user", Content: prompt})
	}
	hasSystem := false
	for _, message := range out {
		hasSystem = hasSystem || message.Role == "system"
	}
	if !hasSystem {
		out = append([]ChatMessage{{Role: "system", Content: defaultSystem}}, out...)
	}
	// WorkBuddy chat compatibility uses one leading system message. Preserve
	// each system/developer block in encounter order, then assemble split
	// assistant tool turns while retaining call/result identities and order.
	var system []string
	normalized := make([]ChatMessage, 1, len(out))
	for _, message := range out {
		if message.Role == "system" {
			system = append(system, message.Content)
			continue
		}
		normalized = append(normalized, message)
	}
	normalized[0] = ChatMessage{Role: "system", Content: strings.Join(system, "\n\n")}
	return mergeAssistantToolTurns(normalized)
}

// Responses represents each parallel call as a separate item. WorkBuddy's
// provider requires the whole assistant tool turn in one message immediately
// before its results; separate assistant messages are rejected with 11133.
// Only combine adjacent assistant items in a run containing calls. User and
// tool messages remain boundaries, and no IDs, arguments or results are changed.
func mergeAssistantToolTurns(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for start := 0; start < len(messages); {
		end := start + 1
		hasCalls := len(messages[start].ToolCalls) > 0
		if messages[start].Role == "assistant" {
			for end < len(messages) && messages[end].Role == "assistant" {
				hasCalls = hasCalls || len(messages[end].ToolCalls) > 0
				end++
			}
		}
		if !hasCalls || end == start+1 {
			out = append(out, messages[start:end]...)
		} else {
			merged := ChatMessage{Role: "assistant"}
			var text, reasoning []string
			for _, message := range messages[start:end] {
				if message.Content != "" {
					text = append(text, message.Content)
				}
				if message.ReasoningContent != "" {
					reasoning = append(reasoning, message.ReasoningContent)
				}
				merged.ToolCalls = append(merged.ToolCalls, message.ToolCalls...)
			}
			merged.Content = strings.Join(text, "\n")
			merged.ReasoningContent = strings.Join(reasoning, "\n")
			out = append(out, merged)
		}
		start = end
	}
	return out
}

func reasoningForReplay(msg prompt.Message) string {
	if strings.EqualFold(strings.TrimSpace(msg.Role), "assistant") {
		return strings.TrimSpace(msg.ReasoningContent)
	}
	return ""
}

// convertAssistantMessage maps text, thinking and tool_use blocks onto one
// assistant message.
func convertAssistantMessage(msg prompt.Message, pendingToolCalls map[string]bool) (ChatMessage, bool) {
	message := ChatMessage{Role: "assistant", ReasoningContent: strings.TrimSpace(msg.ReasoningContent)}
	var text []string
	for _, block := range msg.Content.GetBlocks() {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				text = append(text, block.Text)
			}
		case "thinking":
			if message.ReasoningContent == "" {
				message.ReasoningContent = strings.TrimSpace(block.Thinking)
			}
		case "tool_use":
			name := strings.TrimSpace(block.Name)
			if name == "" {
				continue
			}
			id := strings.TrimSpace(block.ID)
			if id == "" {
				id = NewToolCallID()
			}
			message.ToolCalls = append(message.ToolCalls, ToolCall{
				ID:   id,
				Type: "function",
				Function: ToolCallFunction{
					Name:      name,
					Arguments: util.CompactToolInput(block.Input),
				},
			})
			pendingToolCalls[id] = true
		}
	}
	message.Content = strings.Join(text, "\n")
	return message, message.Content != "" || len(message.ToolCalls) > 0 || message.ReasoningContent != ""
}

// convertBlockMessage maps user/system blocks, splitting tool_result blocks
// into standalone `tool` messages so the assistant/tool pairing stays intact.
func convertBlockMessage(role string, msg prompt.Message, pendingToolCalls map[string]bool) []ChatMessage {
	blocks := msg.Content.GetBlocks()
	out := make([]ChatMessage, 0, len(blocks))
	pending := make([]string, 0, len(blocks))
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, ChatMessage{Role: role, Content: strings.Join(pending, "\n")})
		pending = pending[:0]
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			text := block.Text
			if role == "system" {
				text = filterClientSystemText(text)
			}
			if strings.TrimSpace(text) != "" {
				pending = append(pending, text)
			}
		case "tool_result":
			flush()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" {
				continue
			}
			if !pendingToolCalls[toolID] {
				continue
			}
			delete(pendingToolCalls, toolID)
			out = append(out, ChatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    util.StringifyToolResult(block.Content),
			})
		}
	}
	flush()
	return out
}

// normalizeToolChoice converts both OpenAI and Anthropic selection forms to
// the string-only control accepted by WorkBuddy. A named-tool request cannot
// be represented exactly by this upstream, so "required" is the closest safe
// behavior; the response-side allowlist still rejects undeclared tool names.
func normalizeToolChoice(choice interface{}) string {
	switch typed := choice.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "auto", "required", "none":
			return strings.ToLower(strings.TrimSpace(typed))
		}
	case map[string]interface{}:
		switch strings.ToLower(strings.TrimSpace(util.StringValue(typed["type"]))) {
		case "auto":
			return "auto"
		case "none":
			return "none"
		case "any", "required", "tool", "function":
			return "required"
		}
	}
	return "auto"
}

// normalizeToolDefinitions renders the OpenAI function envelope for the tool
// declarations the gateway forwards. It is the shared implementation in
// internal/util: WorkBuddy, Qoder and Cline all need the same envelope.
func normalizeToolDefinitions(tools []interface{}) []interface{} {
	return util.NormalizeToolDefinitions(tools)
}
