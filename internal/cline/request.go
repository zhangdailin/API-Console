package cline

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"encoding/json"

	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// chatMessage is one OpenAI-shaped message in the Cline payload.
type chatMessage = upstream.ChatMessage

// chatBody is the upstream chat request.
//
// The upstream answers an omitted reasoning_effort with empty content for some
// models, and it needs a session_id to correlate a turn, so both are always
// present even when the client did not ask for them.
type chatBody struct {
	ResponseFormat    map[string]any `json:"response_format,omitempty"`
	PromptCacheKey    string         `json:"prompt_cache_key,omitempty"`
	Temperature       *float64       `json:"temperature,omitempty"`
	TopP              *float64       `json:"top_p,omitempty"`
	Stop              *[]string      `json:"stop,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	Model             string         `json:"model"`
	MaxTokens         int            `json:"max_tokens"`
	SessionID         string         `json:"session_id"`
	ReasoningEffort   string         `json:"reasoning_effort"`
	Messages          []chatMessage  `json:"messages"`
	Stream            bool           `json:"stream"`
	Tools             []any          `json:"tools,omitempty"`
	ToolChoice        any            `json:"tool_choice,omitempty"`
}

// chatPayload is one rendered chat request plus what the client half of the
// stream needs to undo the request-side rewrites.
type chatPayload struct {
	body []byte
	// toolNames maps an upstream tool name back to the client's original one.
	// It is nil when no name had to be shortened.
	toolNames map[string]string
	// defaultMaxTokens reports that max_tokens is this package's default rather
	// than the client's own value, so a repair may lower it.
	defaultMaxTokens bool
}

// buildChatBody renders one chat request.
func buildChatBody(req upstream.UpstreamRequest, model string) ([]byte, error) {
	payload, err := buildChatPayload(req, model)
	return payload.body, err
}

// buildChatPayload renders one chat request and records the rewrites the
// response side has to reverse.
func buildChatPayload(req upstream.UpstreamRequest, model string) (chatPayload, error) {
	if err := req.ValidateProtocolControls("cline"); err != nil {
		return chatPayload{}, err
	}
	sessionID := logicalTaskID(req.RequestID)
	maxTokens := DefaultMaxTokens
	defaultMaxTokens := true
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		// A non-positive budget is not a usable request; it is treated as unset
		// rather than forwarded for the upstream to reject.
		maxTokens = *req.MaxTokens
		defaultMaxTokens = false
	}
	// The upstream needs a reasoning_effort on every request, so the default
	// stands in when the client did not state one; a stated effort wins.
	effort := strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	effort = util.FirstNonEmptyUntrimmed(effort, DefaultReasoningEffort)
	body := chatBody{
		ResponseFormat: req.ChatResponseFormat(), PromptCacheKey: req.PromptCacheKey,
		Temperature: req.Temperature, TopP: req.TopP, ParallelToolCalls: req.ParallelToolCalls,
		Model:           model,
		MaxTokens:       maxTokens,
		SessionID:       sessionID,
		ReasoningEffort: effort,
		Messages:        buildMessages(req),
		Stream:          true,
	}
	if req.Stop != nil {
		body.Stop = &req.Stop
	}
	if !req.NoTools {
		body.Tools = normalizeToolDefinitions(req)
		body.ToolChoice = normalizeToolChoice(req.ToolChoice)
	}
	toolNames := shortenToolNames(&body)
	raw, err := json.Marshal(body)
	if err != nil {
		return chatPayload{}, err
	}
	return chatPayload{body: raw, toolNames: toolNames, defaultMaxTokens: defaultMaxTokens}, nil
}

// maxToolNameLength is the upstream's ceiling on a function name. A longer
// name (common for namespaced MCP tools) is rejected for the whole request.
const maxToolNameLength = 64

// upstreamToolName shortens an over-long name deterministically: the same name
// maps to the same short form on every turn, so a tool call replayed from
// history keeps matching its declaration.
func upstreamToolName(name string) string {
	if len(name) <= maxToolNameLength {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%.55s_%x", name, sum[:4])
}

// shortenToolNames rewrites over-long tool names in the declarations, the
// replayed tool calls and tool_choice, and returns the reverse mapping. It
// copies every map it changes: declarations are shared with the request, which
// a retry on another account renders again.
func shortenToolNames(body *chatBody) map[string]string {
	var names map[string]string
	rename := func(name string) string {
		short := upstreamToolName(name)
		if short != name {
			if names == nil {
				names = make(map[string]string)
			}
			names[short] = name
		}
		return short
	}
	renameFunctionHolder := func(holder map[string]any) map[string]any {
		function, ok := holder["function"].(map[string]any)
		if !ok {
			return holder
		}
		name := util.StringValue(function["name"])
		if short := rename(name); short != name {
			function = maps.Clone(function)
			function["name"] = short
			holder = maps.Clone(holder)
			holder["function"] = function
		}
		return holder
	}
	for i, raw := range body.Tools {
		if tool, ok := raw.(map[string]any); ok {
			body.Tools[i] = renameFunctionHolder(tool)
		}
	}
	if choice, ok := body.ToolChoice.(map[string]any); ok {
		body.ToolChoice = renameFunctionHolder(choice)
	}
	for i := range body.Messages {
		for j := range body.Messages[i].ToolCalls {
			call := &body.Messages[i].ToolCalls[j]
			call.Function.Name = rename(call.Function.Name)
		}
	}
	return names
}

// Request repairs. Each one answers a specific upstream refusal of a request
// that is otherwise well formed, and is applied at most once per call.
const (
	// repairReasoningReplay answers GLM-family models, which refuse a history
	// whose assistant turns carry no reasoning_content ("Missing required
	// parameter: 'messages[N].reasoning_content'"). Most OpenAI clients never
	// send it back, so a placeholder is filled in; an empty string is refused
	// as well.
	repairReasoningReplay = iota
	// repairReasoningEffort drops a reasoning_effort the upstream refused.
	repairReasoningEffort
	// repairMaxTokens lowers this package's default budget when the upstream
	// refused it as too large for the model. A client-stated budget is protocol
	// data and is never changed.
	repairMaxTokens
	repairCount
)

// reasoningReplayPlaceholder is the minimal reasoning text the upstream accepts.
const reasoningReplayPlaceholder = "."

// repairMaxTokensBudget is the budget a refused default is lowered to.
const repairMaxTokensBudget = 32000

// repairKind reports which repair, if any, an upstream refusal asks for.
func repairKind(err error, payload chatPayload) (int, bool) {
	if err == nil {
		return 0, false
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "reasoning_content") && strings.Contains(text, "missing required parameter"):
		return repairReasoningReplay, true
	case strings.Contains(text, "reasoning_effort") || strings.Contains(text, "reasoning effort"):
		return repairReasoningEffort, true
	case payload.defaultMaxTokens && (strings.Contains(text, "max_tokens") ||
		strings.Contains(text, "maxoutputtokens") || strings.Contains(text, "max_output_tokens") ||
		strings.Contains(text, "context length") || strings.Contains(text, "context window")):
		return repairMaxTokens, true
	}
	return 0, false
}

// repairChatBody applies one repair to a rendered body. It reports false when
// the body has nothing the repair could change, so a refusal is not replayed
// unchanged.
func repairChatBody(body []byte, kind int) ([]byte, bool) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, false
	}
	changed := false
	switch kind {
	case repairReasoningReplay:
		messages, _ := decoded["messages"].([]any)
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok || message["role"] != "assistant" {
				continue
			}
			if strings.TrimSpace(util.StringValue(message["reasoning_content"])) == "" {
				message["reasoning_content"] = reasoningReplayPlaceholder
				changed = true
			}
		}
	case repairReasoningEffort:
		if _, ok := decoded["reasoning_effort"]; ok {
			delete(decoded, "reasoning_effort")
			changed = true
		}
	case repairMaxTokens:
		if budget, ok := decoded["max_tokens"].(float64); ok && budget > repairMaxTokensBudget {
			decoded["max_tokens"] = repairMaxTokensBudget
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	repaired, err := json.Marshal(decoded)
	if err != nil {
		return nil, false
	}
	return repaired, true
}

// buildMessages renders the request history.
//
// Cline accepts the OpenAI roles directly, so the mapping is a straight
// projection: system items first, assistant tool_use blocks become tool_calls,
// and a tool_result becomes a `tool` message paired with the call it answers.
func buildMessages(req upstream.UpstreamRequest) []chatMessage {
	out := make([]chatMessage, 0, len(req.Messages)+len(req.System)+2)
	pendingToolCalls := make(map[string]bool)

	for _, item := range req.System {
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		out = append(out, chatMessage{Role: "system", Content: item.Text})
	}

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		switch role {
		case "", "user":
			role = "user"
		case "developer":
			// `developer` is the new name for the system role; the rewrite is
			// lossless and keeps the upstream's role whitelist satisfied.
			role = "system"
		}

		if msg.Content.IsString() {
			text := msg.Content.GetText()
			if strings.TrimSpace(text) == "" {
				// An empty text message carries nothing for the upstream and
				// trips its role/content validation.
				continue
			}
			out = append(out, chatMessage{Role: role, Content: text, ReasoningContent: upstream.ReasoningForReplay(msg)})
			continue
		}

		switch role {
		case "assistant":
			if converted, ok := upstream.ConvertAssistantMessage(msg, pendingToolCalls); ok {
				out = append(out, converted)
			}
		default:
			out = append(out, convertBlockMessage(role, msg, pendingToolCalls)...)
		}
	}

	if len(out) == 0 {
		text := strings.TrimSpace(req.Prompt)
		if text == "" {
			text = "hello"
		}
		out = append(out, chatMessage{Role: "user", Content: text})
	}
	return out
}

// convertBlockMessage maps user/system blocks, splitting tool_result blocks
// into standalone `tool` messages so the assistant/tool pairing stays intact.
func convertBlockMessage(role string, msg prompt.Message, pendingToolCalls map[string]bool) []chatMessage {
	blocks := msg.Content.GetBlocks()
	out := make([]chatMessage, 0, len(blocks))
	pending := make([]string, 0, len(blocks))
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, chatMessage{Role: role, Content: strings.Join(pending, "\n")})
		pending = pending[:0]
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				pending = append(pending, block.Text)
			}
		case "tool_result":
			flush()
			toolID := strings.TrimSpace(block.ToolUseID)
			if toolID == "" || !pendingToolCalls[toolID] {
				// A tool_result with no matching call in this history would be
				// rejected as an orphan; dropping it is the only safe answer.
				continue
			}
			delete(pendingToolCalls, toolID)
			out = append(out, chatMessage{
				Role:       "tool",
				ToolCallID: toolID,
				Content:    toolResultText(block.Content),
			})
		}
	}
	flush()
	return out
}

// toolResultText renders a tool_result payload as text. The upstream's `tool`
// message carries a string, and a structured result is JSON.
func toolResultText(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return typed
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return util.StringValue(typed)
		}
		return string(raw)
	}
}

// normalizeToolDefinitions converts Anthropic declarations into the OpenAI
// function envelope required by Cline. OpenAI declarations are retained as-is.
func normalizeToolDefinitions(req upstream.UpstreamRequest) []any {
	if len(req.Tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(req.Tools))
	for _, raw := range req.Tools {
		tool, ok := raw.(map[string]any)
		if !ok || tool == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(util.StringValue(tool["type"])), "function") {
			if function, ok := tool["function"].(map[string]any); ok && strings.TrimSpace(util.StringValue(function["name"])) != "" {
				out = append(out, tool)
			}
			continue
		}
		name := strings.TrimSpace(util.StringValue(tool["name"]))
		if name == "" {
			continue
		}
		parameters := tool["input_schema"]
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		function := map[string]any{
			"name":       name,
			"parameters": parameters,
		}
		if description := strings.TrimSpace(util.StringValue(tool["description"])); description != "" {
			function["description"] = description
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func normalizeToolChoice(choice any) any {
	object, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	typeName := strings.ToLower(strings.TrimSpace(util.StringValue(object["type"])))
	switch typeName {
	case "auto", "none", "required":
		return typeName
	case "any":
		return "required"
	case "tool", "function":
		name := strings.TrimSpace(util.StringValue(object["name"]))
		if name == "" {
			if function, ok := object["function"].(map[string]any); ok {
				name = strings.TrimSpace(util.StringValue(function["name"]))
			}
		}
		if name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return choice
}

// attemptStreamError marks a rejected credential for the one refresh attempt.
type attemptStreamError struct {
	err    error
	unauth bool
}

func (e *attemptStreamError) Error() string { return e.err.Error() }
func (e *attemptStreamError) Unwrap() error { return e.err }

func isUnauthorized(err error) bool {
	var target *attemptStreamError
	return errors.As(err, &target) && target.unauth
}

// classifyStatus preserves the upstream status error, identifying only a 401 as
// eligible for a credential refresh. A known inference cap carries its cooldown.
func classifyStatus(status int, raw []byte) error {
	detail := strings.TrimSpace(string(raw))
	wrapped := apiError(http.MethodPost, "/chat/completions", status, raw)
	switch status {
	case http.StatusUnauthorized:
		// 401 means the access token is refused. It is the one status a refresh
		// can still repair, so it keeps the unauth flag.
		return &attemptStreamError{err: fmt.Errorf("%w: %v", ErrCredentialMissing, wrapped), unauth: true}
	case http.StatusForbidden:
		// 403 is not a credential verdict here. The upstream answers it for an
		// entitlement the account does not hold — an unsubscribed model, or a
		// catalog entry this plan may not run — and the message names the model,
		// not the token. Reporting it as a missing credential would take a
		// healthy account out of rotation and demand a re-login that cannot fix
		// it, so it is surfaced as a plain refusal for the scheduler to judge.
		return &attemptStreamError{err: wrapped}
	case http.StatusTooManyRequests:
		// The inference cap names the account and states its own recovery
		// window, so it is returned as-is: the scheduler cools the account for
		// the stated duration instead of retrying blind.
		if capErr := inferenceCapError(detail); capErr.Wait > 0 ||
			strings.Contains(strings.ToLower(detail), "inference cap") ||
			strings.Contains(detail, "INFERENCE_CAP") ||
			strings.Contains(detail, "Try again in") {
			return capErr
		}
	}
	return &attemptStreamError{err: wrapped}
}
