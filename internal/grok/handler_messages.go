package grok

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
	"slices"
	"strings"
	"time"

	"encoding/json"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

type anthropicMessagesRequest struct {
	Model         string                   `json:"model"`
	MaxTokens     int                      `json:"max_tokens"`
	Messages      []anthropicMessage       `json:"messages"`
	System        interface{}              `json:"system,omitempty"`
	Tools         []anthropicTool          `json:"tools,omitempty"`
	ToolChoice    interface{}              `json:"tool_choice,omitempty"`
	Stream        bool                     `json:"stream,omitempty"`
	Temperature   *float64                 `json:"temperature,omitempty"`
	TopP          *float64                 `json:"top_p,omitempty"`
	Metadata      map[string]interface{}   `json:"metadata,omitempty"`
	Thinking      map[string]interface{}   `json:"thinking,omitempty"`
	StopSequences []string                 `json:"stop_sequences,omitempty"`
	OutputConfig  map[string]interface{}   `json:"output_config,omitempty"`
	MCPServers    []map[string]interface{} `json:"mcp_servers,omitempty"`
}

type anthropicMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type anthropicTool struct {
	Type            string      `json:"type,omitempty"`
	Name            string      `json:"name"`
	Description     string      `json:"description,omitempty"`
	InputSchema     interface{} `json:"input_schema"`
	Strict          *bool       `json:"strict,omitempty"`
	AllowedDomains  []string    `json:"allowed_domains,omitempty"`
	BlockedDomains  []string    `json:"blocked_domains,omitempty"`
	ExcludedDomains []string    `json:"excluded_domains,omitempty"`
}

// HandleMessages exposes an Anthropic Messages compatibility surface for the
// Grok channel while preserving the existing Chat routing and account logic.
func (h *Handler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req anthropicMessagesRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Model) == "" || len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "model and messages are required")
		return
	}
	req.Model = normalizeModelID(req.Model)
	if !middleware.APIKeyAllowsModel(r.Context(), req.Model) {
		writeAnthropicError(w, http.StatusForbidden, "API key is not allowed to use model "+req.Model)
		return
	}
	if req.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "max_tokens must be greater than zero")
		return
	}
	spec, ok := h.resolveConversationModel(r.Context(), req.Model)
	if !ok {
		writeAnthropicModelNotFound(w, req.Model)
		return
	}
	if err := h.ensureResolvedModelCapability(r.Context(), spec.ID, spec, store.CapabilityMessages); err != nil {
		if strings.EqualFold(strings.TrimSpace(err.Error()), "model not found") {
			writeAnthropicModelNotFound(w, req.Model)
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	chat, err := anthropicRequestToChat(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error())
		return
	}
	body, err := json.Marshal(chat)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "failed to encode request")
		return
	}
	subReq := r.Clone(context.WithValue(r.Context(), chatSourceOperationKey{}, "messages"))
	subReq.Method = http.MethodPost
	subReq.URL.Path = "/grok/v1/chat/completions"
	subReq.Header = r.Header.Clone()
	subReq.Header.Set("Content-Type", "application/json")
	subReq.Body = io.NopCloser(bytes.NewReader(body))
	subReq.ContentLength = int64(len(body))

	if req.Stream {
		prompt := estimatePromptUsageFromRequest(&chat)
		inputTokens := prompt.promptTextTokens + prompt.promptAudioTokens + prompt.promptImageTokens
		h.serveAnthropicMessageStream(w, subReq, req.Model, inputTokens)
		return
	}
	rec := newCaptureResponseWriter()
	h.HandleChatCompletions(rec, subReq)
	if rec.code < 200 || rec.code >= 300 {
		writeAnthropicUpstreamError(w, rec.code, rec.body.String())
		return
	}
	var chatResponse map[string]interface{}
	if err := json.Unmarshal(rec.body.Bytes(), &chatResponse); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "invalid chat response")
		return
	}
	util.WriteJSON(w, anthropicResponseFromChat(req.Model, chatResponse))
}

func anthropicRequestToChat(req anthropicMessagesRequest) (chatwire.Request, error) {
	for name, value := range map[string]*float64{"temperature": req.Temperature, "top_p": req.TopP} {
		if value != nil && (*value < 0 || *value > 1) {
			return chatwire.Request{}, fmt.Errorf("%s must be between 0 and 1", name)
		}
	}
	for index, sequence := range req.StopSequences {
		if sequence == "" {
			return chatwire.Request{}, fmt.Errorf("stop_sequences[%d] must not be empty", index)
		}
	}
	// A message-level system/developer role is not part of the Anthropic wire
	// contract but some clients (and OpenAI-style payloads relayed through this
	// endpoint) send it. Fold it into the instructions instead of rejecting the
	// whole request.
	systemText := anthropicSystemText(req.System)
	for _, message := range req.Messages {
		if !isAnthropicInstructionRole(message.Role) {
			continue
		}
		if text := strings.TrimSpace(anthropicBlockContentText(message.Content)); text != "" {
			if systemText != "" {
				systemText += "\n\n"
			}
			systemText += text
		}
	}
	messages := make([]chatwire.Message, 0, len(req.Messages)+1)
	if systemText != "" {
		messages = append(messages, chatwire.Message{Role: "system", Content: systemText})
	}
	for _, message := range req.Messages {
		if isAnthropicInstructionRole(message.Role) {
			continue
		}
		converted, err := anthropicMessageToChat(message)
		if err != nil {
			return chatwire.Request{}, err
		}
		messages = append(messages, converted...)
	}
	if err := validateChatToolSequence(messages); err != nil {
		return chatwire.Request{}, err
	}
	tools := make([]chatwire.ToolDef, 0, len(req.Tools))
	nativeTools := make([]map[string]interface{}, 0, len(req.Tools)+len(req.MCPServers))
	for _, tool := range req.Tools {
		typeName := strings.ToLower(strings.TrimSpace(tool.Type))
		if strings.HasPrefix(typeName, "web_search_") {
			search, err := anthropicSearchTool(tool)
			if err != nil {
				return chatwire.Request{}, err
			}
			nativeTools = append(nativeTools, search)
			continue
		}
		if typeName != "" && typeName != "custom" {
			return chatwire.Request{}, fmt.Errorf("unsupported Anthropic server tool type=%q", tool.Type)
		}
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			return chatwire.Request{}, fmt.Errorf("tool name is required")
		}
		parameters := tool.InputSchema
		if parameters == nil {
			parameters = map[string]interface{}{"type": "object"}
		}
		function := map[string]interface{}{
			"name": name, "description": tool.Description, "parameters": parameters,
		}
		if tool.Strict != nil {
			function["strict"] = *tool.Strict
		}
		tools = append(tools, chatwire.ToolDef{Type: "function", Function: function})
	}
	for index, server := range req.MCPServers {
		name := strings.TrimSpace(fmt.Sprint(server["name"]))
		url := strings.TrimSpace(fmt.Sprint(server["url"]))
		if name == "" || name == "<nil>" || url == "" || url == "<nil>" {
			return chatwire.Request{}, fmt.Errorf("mcp_servers[%d] requires name and url", index)
		}
		item := map[string]interface{}{"type": "mcp", "server_label": name, "server_url": url}
		if token := strings.TrimSpace(fmt.Sprint(server["authorization_token"])); token != "" && token != "<nil>" {
			item["authorization"] = token
		}
		nativeTools = append(nativeTools, item)
	}
	maxTokens := req.MaxTokens
	reasoningEffort, wantsReasoningSummary := anthropicReasoningEffort(req.Thinking, req.OutputConfig)
	promptCacheKey := anthropicPromptCacheKey(req.Metadata)
	var responseText map[string]interface{}
	if format, _ := req.OutputConfig["format"].(map[string]interface{}); len(format) > 0 {
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(format["type"])), "json_schema") || format["schema"] == nil {
			return chatwire.Request{}, fmt.Errorf("output_config.format must be json_schema with schema")
		}
		responseText = map[string]interface{}{"format": map[string]interface{}{"type": "json_schema", "name": "anthropic_output", "schema": format["schema"]}}
	}
	parallel := anthropicParallelToolCalls(req.ToolChoice)
	responsesInput, err := anthropicResponsesInput(req.Messages)
	if err != nil {
		return chatwire.Request{}, err
	}
	// A thinking request must also ask the upstream for a summary: without it
	// the streamed thinking block would carry only a signature and the client
	// (Claude Code included) would show no reasoning text at all, so a thinking
	// request also asks upstream for a detailed summary.
	var reasoningSummary *string
	if wantsReasoningSummary {
		summary := "detailed"
		reasoningSummary = &summary
	}
	return chatwire.Request{
		SourceOperation:   "messages",
		Model:             req.Model,
		Messages:          messages,
		Stream:            req.Stream,
		StreamProvided:    true,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		MaxTokens:         &maxTokens,
		Tools:             tools,
		ResponsesTools:    nativeTools,
		ResponsesInput:    responsesInput,
		ToolChoice:        anthropicToolChoiceToOpenAI(req.ToolChoice),
		ParallelToolCalls: parallel,
		ReasoningEffort:   reasoningEffort,
		ReasoningSummary:  reasoningSummary,
		Stop:              append([]string(nil), req.StopSequences...),
		PromptCacheKey:    promptCacheKey,
		SafetyIdentifier:  chatwire.ParseLooseStringAny(req.Metadata["user_id"]),
		ResponseText:      responseText,
	}, nil
}

func anthropicResponsesInput(messages []anthropicMessage) ([]interface{}, error) {
	input := make([]interface{}, 0, len(messages))
	serverSearches := map[string]map[string]interface{}{}
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if text, ok := message.Content.(string); ok {
			if strings.TrimSpace(text) != "" {
				partType := "input_text"
				if role == "assistant" {
					partType = "output_text"
				}
				input = append(input, map[string]interface{}{"type": "message", "role": role, "content": []interface{}{map[string]interface{}{"type": partType, "text": text}}})
			}
			continue
		}
		blocks, _ := message.Content.([]interface{})
		parts := make([]interface{}, 0, len(blocks))
		flush := func() {
			if len(parts) == 0 {
				return
			}
			input = append(input, map[string]interface{}{"type": "message", "role": role, "content": parts})
			parts = nil
		}
		for _, raw := range blocks {
			block, _ := raw.(map[string]interface{})
			kind := strings.ToLower(strings.TrimSpace(fmt.Sprint(block["type"])))
			switch kind {
			case "text":
				partType := "input_text"
				if role == "assistant" {
					partType = "output_text"
				}
				parts = append(parts, map[string]interface{}{"type": partType, "text": fmt.Sprint(block["text"])})
			case "image":
				if url := anthropicSourceURL(block["source"]); url != "" {
					parts = append(parts, map[string]interface{}{"type": "input_image", "image_url": url})
				}
			case "document":
				document, err := anthropicDocumentContent(block)
				if err != nil {
					return nil, err
				}
				parts = append(parts, document)
			case "tool_use":
				flush()
				input = append(input, map[string]interface{}{
					"type": "function_call", "call_id": chatwire.ParseLooseStringAny(block["id"]),
					"name": chatwire.ParseLooseStringAny(block["name"]), "arguments": stringifyToolArguments(block["input"]),
				})
			case "tool_result":
				flush()
				output := anthropicToolResultContent(block["content"])
				if isError, _ := block["is_error"].(bool); isError {
					output = prependAnthropicToolError(output)
				}
				input = append(input, map[string]interface{}{"type": "function_call_output", "call_id": chatwire.ParseLooseStringAny(block["tool_use_id"]), "output": output})
			case "thinking", "redacted_thinking":
				flush()
				reasoning := map[string]interface{}{"type": "reasoning", "summary": []interface{}{}}
				if kind == "thinking" {
					reasoning["summary"] = []interface{}{map[string]interface{}{"type": "summary_text", "text": chatwire.ParseLooseStringAny(block["thinking"])}}
				}
				if encrypted := chatwire.ParseLooseStringAny(firstDefined(block["signature"], block["data"])); encrypted != "" {
					reasoning["encrypted_content"] = encrypted
				}
				input = append(input, reasoning)
			case "server_tool_use":
				if role != "assistant" || !strings.EqualFold(chatwire.ParseLooseStringAny(block["name"]), "web_search") {
					continue
				}
				flush()
				id := chatwire.ParseLooseStringAny(block["id"])
				arguments, _ := block["input"].(map[string]interface{})
				call := map[string]interface{}{
					"type": "web_search_call", "id": id, "status": "completed",
					"action": map[string]interface{}{"type": "search", "query": chatwire.ParseLooseStringAny(arguments["query"])},
				}
				serverSearches[id] = call
				input = append(input, call)
			case "web_search_tool_result":
				call := serverSearches[chatwire.ParseLooseStringAny(block["tool_use_id"])]
				if call != nil {
					applyAnthropicWebSearchResult(call, block["content"])
				}
			}
		}
		flush()
	}
	return input, nil
}

func applyAnthropicWebSearchResult(call map[string]interface{}, raw interface{}) {
	if call == nil {
		return
	}
	results, ok := raw.([]interface{})
	if ok {
		sources := make([]interface{}, 0, len(results))
		for _, item := range results {
			result, _ := item.(map[string]interface{})
			if strings.EqualFold(chatwire.ParseLooseStringAny(result["type"]), "web_search_result") {
				if value := chatwire.ParseLooseStringAny(result["url"]); value != "" {
					sources = append(sources, map[string]interface{}{"type": "url", "url": value})
				}
			}
		}
		if action, _ := call["action"].(map[string]interface{}); action != nil && len(sources) > 0 {
			action["sources"] = sources
		}
		return
	}
	if result, _ := raw.(map[string]interface{}); result != nil && strings.EqualFold(chatwire.ParseLooseStringAny(result["type"]), "web_search_tool_result_error") {
		call["status"] = "failed"
	}
}

func validateChatToolSequence(messages []chatwire.Message) error {
	pending := make(map[string]bool)
	completed := make(map[string]bool)
	for _, message := range messages {
		if strings.EqualFold(strings.TrimSpace(message.Role), "assistant") {
			for _, call := range message.ToolCalls {
				id := strings.TrimSpace(call.ID)
				if id == "" {
					return fmt.Errorf("tool_use id is required")
				}
				if pending[id] || completed[id] {
					return fmt.Errorf("duplicate tool_use id %q", id)
				}
				pending[id] = true
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(message.Role), "tool") {
			continue
		}
		id := strings.TrimSpace(message.ToolCallID)
		// A completed id was deleted from pending, so it already fails here; an
		// explicit duplicate-result check would be unreachable.
		if id == "" || !pending[id] {
			return fmt.Errorf("tool_result references unknown tool_use_id %q", id)
		}
		delete(pending, id)
		completed[id] = true
	}
	// Every tool_use must be answered: an unanswered call is forwarded upstream
	// as a dangling call_id, which the upstream rejects much later and with a
	// message that does not name the offending call.
	if len(pending) > 0 {
		unanswered := slices.Sorted(maps.Keys(pending))
		return fmt.Errorf("tool_use %s has no matching tool_result", strings.Join(unanswered, ", "))
	}
	return nil
}

func anthropicReasoningEffort(thinking, outputConfig map[string]interface{}) (*string, bool) {
	// An explicit `thinking: {"type":"disabled"}` wins over output_config: a
	// client that asked for no reasoning must not silently get reasoning just
	// because a stale effort value was also present; resolve in that order.
	typeName := strings.ToLower(strings.TrimSpace(fmt.Sprint(thinking["type"])))
	// A thinking request must also ask the upstream for a summary: without it the
	// streamed thinking block would carry only a signature and the client (Claude
	// Code included) would show no reasoning text at all.
	thinkingRequested := typeName == "enabled" || typeName == "adaptive"
	if typeName == "disabled" {
		effort := "none"
		return &effort, false
	}
	if effort := strings.ToLower(strings.TrimSpace(fmt.Sprint(outputConfig["effort"]))); effort != "" && effort != "<nil>" {
		return &effort, thinkingRequested
	}
	if thinkingRequested {
		budget, _ := chatwire.ParseLooseIntAny(thinking["budget_tokens"])
		effort := "medium"
		if budget > 0 && budget <= 2048 {
			effort = "low"
		} else if budget > 10000 {
			effort = "high"
		}
		if configured := strings.ToLower(strings.TrimSpace(fmt.Sprint(thinking["effort"]))); configured != "" && configured != "<nil>" {
			effort = configured
		}
		return &effort, true
	}
	return nil, false
}

func anthropicPromptCacheKey(metadata map[string]interface{}) string {
	for _, key := range []string{"prompt_cache_key", "session_id", "user_id"} {
		if value := strings.TrimSpace(fmt.Sprint(metadata[key])); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func cloneStringInterfaceMap(value map[string]interface{}) map[string]interface{} {
	if len(value) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(value))
	for key, item := range value {
		out[key] = item
	}
	return out
}

func anthropicSystemText(value interface{}) string {
	switch v := value.(type) {
	case string:
		return stripAnthropicBillingHeader(v)
	case []interface{}:
		parts := make([]string, 0, len(v))
		for _, raw := range v {
			block, _ := raw.(map[string]interface{})
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(block["type"])), "text") {
				if text := stripAnthropicBillingHeader(fmt.Sprint(block["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

// stripAnthropicBillingHeader removes the per-request Claude Code billing
// header. It changes on every request, and because the system prompt sits at the
// very front of the upstream prefix, keeping it defeats the provider's prompt
// cache (the gateway strips it for exactly that reason).
func stripAnthropicBillingHeader(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || !strings.Contains(strings.ToLower(trimmed), "x-anthropic-billing-header") {
		return trimmed
	}
	kept := make([]string, 0, strings.Count(trimmed, "\n")+1)
	for _, line := range strings.Split(trimmed, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "x-anthropic-billing-header") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func anthropicMessageToChat(message anthropicMessage) ([]chatwire.Message, error) {
	role := strings.ToLower(strings.TrimSpace(message.Role))
	if role != "user" && role != "assistant" {
		return nil, fmt.Errorf("unsupported message role %q", message.Role)
	}
	if text, ok := message.Content.(string); ok {
		return []chatwire.Message{{Role: role, Content: text}}, nil
	}
	blocks, ok := message.Content.([]interface{})
	if !ok {
		return nil, fmt.Errorf("message content must be a string or array")
	}
	content := make([]interface{}, 0, len(blocks))
	toolCalls := make([]chatwire.ToolCall, 0)
	toolResults := make([]chatwire.Message, 0)
	var reasoningContent, reasoningEncryptedContent string
	for _, raw := range blocks {
		block, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(fmt.Sprint(block["type"]))) {
		case "text":
			content = append(content, map[string]interface{}{"type": "text", "text": fmt.Sprint(block["text"])})
		case "image":
			url := anthropicSourceURL(block["source"])
			if url == "" {
				return nil, fmt.Errorf("image source is invalid")
			}
			content = append(content, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}})
		case "document":
			document, err := anthropicDocumentContent(block)
			if err != nil {
				return nil, fmt.Errorf("document source is invalid")
			}
			content = append(content, document)
		case "tool_use":
			if role != "assistant" {
				return nil, fmt.Errorf("tool_use is only valid in assistant messages")
			}
			id, idOK := block["id"].(string)
			name, nameOK := block["name"].(string)
			input, inputOK := block["input"].(map[string]interface{})
			if !idOK || !nameOK || !inputOK || input == nil || strings.TrimSpace(id) == "" || id == "<nil>" || strings.TrimSpace(name) == "" || name == "<nil>" {
				return nil, fmt.Errorf("tool_use requires a non-empty string id, name and object input")
			}
			toolCalls = append(toolCalls, chatwire.ToolCall{
				ID:       strings.TrimSpace(id),
				Type:     "function",
				Function: map[string]interface{}{"name": block["name"], "arguments": block["input"]},
			})
		case "server_tool_use":
			if role != "assistant" {
				return nil, fmt.Errorf("server_tool_use is only valid in assistant messages")
			}
			if strings.EqualFold(strings.TrimSpace(fmt.Sprint(block["name"])), "web_search") {
				input, _ := block["input"].(map[string]interface{})
				content = append(content, map[string]interface{}{"type": "text", "text": "Web search performed: " + strings.TrimSpace(fmt.Sprint(input["query"]))})
			}
		case "tool_result":
			if role != "user" {
				return nil, fmt.Errorf("tool_result is only valid in user messages")
			}
			resultID, validID := block["tool_use_id"].(string)
			if !validID || strings.TrimSpace(resultID) == "" || resultID == "<nil>" {
				return nil, fmt.Errorf("tool_result requires a non-empty string tool_use_id")
			}
			resultContent := anthropicToolResultContent(block["content"])
			if isError, _ := block["is_error"].(bool); isError {
				resultContent = prependAnthropicToolError(resultContent)
			}
			toolResults = append(toolResults, chatwire.Message{
				Role: "tool", ToolCallID: strings.TrimSpace(resultID),
				Content: resultContent,
			})
		case "web_search_tool_result":
			if role == "assistant" {
				if text := anthropicBlockContentText(block["content"]); text != "" {
					content = append(content, map[string]interface{}{"type": "text", "text": text})
				}
			}
		case "thinking", "redacted_thinking":
			if role != "assistant" {
				return nil, fmt.Errorf("thinking is only valid in assistant messages")
			}
			if text := strings.TrimSpace(fmt.Sprint(block["thinking"])); text != "" && text != "<nil>" {
				reasoningContent = text
			}
			if signature := strings.TrimSpace(fmt.Sprint(firstDefined(block["signature"], block["data"]))); signature != "" && signature != "<nil>" {
				reasoningEncryptedContent = signature
			}
		}
	}
	out := make([]chatwire.Message, 0, 1+len(toolResults))
	// Anthropic commonly places tool_result blocks before any follow-up user
	// text in the same message. OpenAI requires the corresponding tool-role
	// messages to precede that user message.
	out = append(out, toolResults...)
	if len(content) > 0 || len(toolCalls) > 0 || reasoningContent != "" || reasoningEncryptedContent != "" {
		var normalizedContent interface{} = content
		if len(content) == 0 {
			normalizedContent = ""
		}
		message := chatwire.Message{Role: role, Content: normalizedContent, ToolCalls: toolCalls,
			ReasoningContent: reasoningContent, ReasoningEncryptedContent: reasoningEncryptedContent}
		out = append(out, message)
	}
	if len(out) == 0 {
		out = append(out, chatwire.Message{Role: role, Content: ""})
	}
	return out, nil
}

func anthropicSourceURL(raw interface{}) string {
	source, _ := raw.(map[string]interface{})
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(source["type"]))) {
	case "base64":
		mediaType := strings.TrimSpace(fmt.Sprint(source["media_type"]))
		data := strings.TrimSpace(fmt.Sprint(source["data"]))
		if mediaType != "" && data != "" {
			return "data:" + mediaType + ";base64," + data
		}
	case "url":
		return strings.TrimSpace(fmt.Sprint(source["url"]))
	}
	return ""
}

func anthropicDocumentContent(block map[string]interface{}) (map[string]interface{}, error) {
	source, _ := block["source"].(map[string]interface{})
	title := chatwire.ParseLooseStringAny(block["title"])
	switch strings.ToLower(chatwire.ParseLooseStringAny(source["type"])) {
	case "text":
		if data := chatwire.ParseLooseStringAny(source["data"]); data != "" {
			return map[string]interface{}{"type": "input_text", "text": data}, nil
		}
	case "url":
		if url := chatwire.ParseLooseStringAny(source["url"]); url != "" {
			out := map[string]interface{}{"type": "input_file", "file_url": url}
			if title != "" {
				out["filename"] = title
			}
			return out, nil
		}
	case "base64":
		mediaType := chatwire.ParseLooseStringAny(source["media_type"])
		data := chatwire.ParseLooseStringAny(source["data"])
		if mediaType != "" && data != "" {
			out := map[string]interface{}{"type": "input_file", "file_data": "data:" + mediaType + ";base64," + data}
			if title != "" {
				out["filename"] = title
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("unsupported document source")
}

func anthropicToolResultContent(raw interface{}) interface{} {
	if text, ok := raw.(string); ok {
		return text
	}
	blocks, _ := raw.([]interface{})
	parts := make([]interface{}, 0, len(blocks))
	for _, value := range blocks {
		block, _ := value.(map[string]interface{})
		switch strings.ToLower(chatwire.ParseLooseStringAny(block["type"])) {
		case "text":
			parts = append(parts, map[string]interface{}{"type": "input_text", "text": fmt.Sprint(block["text"])})
		case "image":
			if url := anthropicSourceURL(block["source"]); url != "" {
				parts = append(parts, map[string]interface{}{"type": "input_image", "detail": "auto", "image_url": url})
			}
		case "document":
			if document, err := anthropicDocumentContent(block); err == nil {
				parts = append(parts, document)
			}
		case "tool_reference":
			if name := chatwire.ParseLooseStringAny(block["tool_name"]); name != "" {
				parts = append(parts, map[string]interface{}{"type": "input_text", "text": fmt.Sprintf("Tool search matched declared tool %q; its definition is available in this request.", name)})
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return parts
}

func prependAnthropicToolError(content interface{}) interface{} {
	const prefix = "Tool execution failed: "
	if text, ok := content.(string); ok {
		return prefix + text
	}
	parts, _ := content.([]interface{})
	return append([]interface{}{map[string]interface{}{"type": "input_text", "text": prefix}}, parts...)
}

func anthropicBlockContentText(raw interface{}) string {
	if text, ok := raw.(string); ok {
		return text
	}
	blocks, _ := raw.([]interface{})
	parts := make([]string, 0, len(blocks))
	for _, item := range blocks {
		block, _ := item.(map[string]interface{})
		if text := fmt.Sprint(block["text"]); text != "<nil>" && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func anthropicToolChoiceToOpenAI(raw interface{}) interface{} {
	choice, ok := raw.(map[string]interface{})
	if !ok {
		return raw
	}
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(choice["type"]))) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": choice["name"]}}
	default:
		return nil
	}
}

func anthropicParallelToolCalls(raw interface{}) *bool {
	choice, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}
	parallel := true
	if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
		parallel = !disabled
	}
	return &parallel
}

func anthropicResponseFromChat(model string, chat map[string]interface{}) map[string]interface{} {
	content := make([]interface{}, 0)
	stopReason := "end_turn"
	sawRefusal := false
	stopSequence := ""
	choices, _ := chat["choices"].([]interface{})
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		message, _ := choice["message"].(map[string]interface{})
		stopSequence = interfaceString(message["stop_sequence"])
		thinking := strings.TrimSpace(fmt.Sprint(firstDefined(message["reasoning_content"], message["reasoning"])))
		signature := strings.TrimSpace(fmt.Sprint(message["reasoning_encrypted_content"]))
		if items := interfaceSlice(message["x_grok_reasoning"]); len(items) > 0 {
			for _, item := range items {
				wrapper := map[string]interface{}{"output": []interface{}{item}}
				content = append(content, map[string]interface{}{"type": "thinking", "thinking": consoleExtractReasoningText(wrapper), "signature": consoleExtractEncryptedReasoning(wrapper)})
			}
			thinking = ""
			signature = ""
		}
		// fmt.Sprint of a missing value is "<nil>"; normalizing it here lets both
		// the presence check and the emitted block use one notion of "absent".
		if thinking == "<nil>" {
			thinking = ""
		}
		if signature == "<nil>" {
			signature = ""
		}
		if thinking != "" || signature != "" {
			block := map[string]interface{}{"type": "thinking", "thinking": thinking}
			if signature != "" {
				block["signature"] = signature
			}
			content = append(content, block)
		}
		for _, raw := range interfaceSlice(message["x_grok_searches"]) {
			item, _ := raw.(map[string]interface{})
			content = append(content, searchContent(item)...)
		}
		if text, ok := message["content"].(string); ok && text != "" {
			block := map[string]interface{}{"type": "text", "text": text}
			if citations := chatCitations(interfaceSlice(message["annotations"])); len(citations) > 0 {
				block["citations"] = citations
			}
			content = append(content, block)
		}
		toolCalls, _ := message["tool_calls"].([]interface{})
		if refusal := streamString(message["refusal"]); refusal != "" {
			content = append(content, map[string]interface{}{"type": "text", "text": refusal})
			sawRefusal = true
		}
		for _, raw := range toolCalls {
			call, _ := raw.(map[string]interface{})
			fn, _ := call["function"].(map[string]interface{})
			input := interface{}(map[string]interface{}{})
			switch args := fn["arguments"].(type) {
			case string:
				_ = json.Unmarshal([]byte(args), &input)
			case nil:
			default:
				input = args
			}
			content = append(content, map[string]interface{}{
				"type": "tool_use", "id": call["id"], "name": fn["name"], "input": input,
			})
		}
		stopReason = openAIFinishToAnthropic(fmt.Sprint(choice["finish_reason"]))
	}
	if sawRefusal && stopReason == "end_turn" {
		stopReason = "refusal"
	}
	if stopSequence != "" {
		stopReason = "stop_sequence"
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	usage, _ := chat["usage"].(map[string]interface{})
	return map[string]interface{}{
		// The id the Chat Completions layer produced is a chatcmpl_* value; the
		// Anthropic contract uses msg_*. Handing the caller a chatcmpl_ message
		// id broke every client that validates the prefix and lost the upstream
		// identity, so an Anthropic-shaped id is generated instead.
		"id":   anthropicMessageID(interfaceString(chat["id"])),
		"type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReason, "stop_sequence": nullableProtocolString(stopSequence),
		"usage": anthropicUsageFromOpenAI(usage),
	}
}

// anthropicMessageID returns an Anthropic-shaped message id.
//
// The internal relay answers with a Chat Completions id (chatcmpl_*). That is
// not an Anthropic message id: a client that validates the prefix rejects it,
// and the upstream identity is lost. Such an id is re-shaped; any other
// upstream id is kept as-is so callers can correlate with the upstream.
func anthropicMessageID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "msg_" + randomHex(12)
	}
	if strings.HasPrefix(trimmed, "chatcmpl_") {
		return "msg_" + strings.TrimPrefix(trimmed, "chatcmpl_")
	}
	return trimmed
}

func firstDefined(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// interfaceString delegates to parseLooseStringAny so the package keeps its short
// local name without duplicating the logic: nil and missing values read as "".
func interfaceString(value interface{}) string { return chatwire.ParseLooseStringAny(value) }

func openAIFinishToAnthropic(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "tool_calls", "tool_use":
		return "tool_use"
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter", "refusal":
		// A safety refusal is not a normal completion; Anthropic has a dedicated
		// stop reason for it, and a caller that only sees end_turn treats the
		// refusal as a real answer.
		return "refusal"
	default:
		// "stop", "end_turn", empty and any unknown reason complete normally.
		return "end_turn"
	}
}

func anthropicUsageFromOpenAI(usage map[string]interface{}) map[string]interface{} {
	input := max(0, interfaceToInt(usage["prompt_tokens"]))
	cached := 0
	if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
		cached = max(0, interfaceToInt(details["cached_tokens"]))
	}
	cached = min(cached, input)
	output := max(0, interfaceToInt(usage["completion_tokens"]))
	result := map[string]interface{}{
		"input_tokens": input - cached,
		// Anthropic reports both cache counters unconditionally, with 0 meaning
		// "no cache". Omitting them made a caller unable to tell "no cache" from
		// "field missing" (Claude Code's /cost and cache statistics read them).
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
		"output_tokens":               output,
	}
	reasoning := 0
	if details, ok := usage["completion_tokens_details"].(map[string]interface{}); ok {
		reasoning = max(0, interfaceToInt(details["reasoning_tokens"]))
	}
	result["output_tokens_details"] = map[string]interface{}{
		"thinking_tokens": min(output, reasoning),
	}
	if cost := interfaceToInt(usage["cost_in_usd_ticks"]); cost > 0 {
		result["cost_in_usd_ticks"] = cost
	}
	if sources := interfaceToInt(firstDefined(usage["num_sources_used"], usage["num_server_side_tools_used"])); sources > 0 {
		result["num_sources_used"] = sources
	}
	return result
}

func (h *Handler) serveAnthropicMessageStream(w http.ResponseWriter, req *http.Request, model string, inputTokens int) {
	h.withChatStream(req, func(status int, header http.Header, reader io.Reader) {
		for key, values := range header {
			w.Header()[key] = values
		}
		if status < 200 || status >= 300 {
			body, _ := io.ReadAll(reader)
			writeAnthropicUpstreamError(w, status, string(body))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_ = translateOpenAIChatStreamToAnthropicWithInput(w, reader, model, inputTokens)
	})
}

type anthropicStreamState struct {
	id string
	// sawRefusal marks that the model refused the request, so a normal "stop"
	// finish reason is reported as the dedicated refusal stop reason.
	sawRefusal       bool
	model            string
	nextIndex        int
	textIndex        int
	thinkIndex       int
	toolIndexes      map[int]int
	open             map[int]bool
	usage            map[string]interface{}
	stopReason       string
	stopSequence     string
	reasoningID      string
	toolIDs          map[string]int
	searches         map[string]*messageSearchState
	citations        map[string]bool
	reasoningIndexes map[string]int
	reasoningSigned  map[int]bool
}

func translateOpenAIChatStreamToAnthropicWithInput(w io.Writer, reader io.Reader, model string, inputTokens int) error {
	tracked := &checkedStreamWriter{Target: w}
	w = tracked
	state := &anthropicStreamState{
		id: "msg_" + randomHex(12), model: model, textIndex: -1, thinkIndex: -1,
		toolIndexes: map[int]int{}, open: map[int]bool{},
		// Named thinking blocks stay addressable by their upstream id until they
		// are signed, so the signature of a late snapshot attaches to the original
		// block instead of an empty extra one.
		reasoningIndexes: map[string]int{}, reasoningSigned: map[int]bool{},
		// Upstream usage arrives on the terminal chunk, after message_start. The
		// request is already parsed here, so seed Anthropic's required input/cache
		// counters from the same conservative estimator used by chat fallback.
		usage:    anthropicUsageFromOpenAI(map[string]interface{}{"prompt_tokens": max(0, inputTokens), "completion_tokens": 0}),
		searches: map[string]*messageSearchState{}, citations: map[string]bool{},
	}
	writeAnthropicSSE(w, "message_start", map[string]interface{}{
		"type": "message_start", "message": map[string]interface{}{"id": state.id, "type": "message", "role": "assistant", "model": model, "created_at": time.Now().Unix(),
			"content": []interface{}{}, "stop_reason": nil, "stop_sequence": nil, "usage": state.usage},
	})
	if tracked.Err != nil {
		return tracked.Err
	}
	terminal := false
	err := responses.ReadSSE(reader, func(event, data string) error {
		if tracked.Err != nil {
			return tracked.Err
		}
		if data == "[DONE]" {
			if !terminal {
				return fmt.Errorf("chat stream ended without finish_reason")
			}
			return io.EOF
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return fmt.Errorf("invalid chat SSE: %w", err)
		}
		if chunk["error"] != nil || event == "error" {
			return responses.Failure(chunk)
		}
		if usage, ok := chunk["usage"].(map[string]interface{}); ok {
			state.usage = anthropicUsageFromOpenAI(usage)
		}
		for _, rawChoice := range interfaceSlice(chunk["choices"]) {
			choice, _ := rawChoice.(map[string]interface{})
			delta, _ := choice["delta"].(map[string]interface{})
			if len(delta) == 0 {
				delta, _ = choice["message"].(map[string]interface{})
			}
			if key := interfaceString(delta["reasoning_item_id"]); key != "" && key != state.reasoningID {
				state.detachThinking(w)
				state.reasoningID = key
			}
			if thinking := streamString(firstDefined(delta["reasoning_content"], delta["reasoning"])); thinking != "" {
				state.writeThinkingDelta(w, "thinking_delta", "thinking", thinking)
			}
			if signature := streamString(delta["reasoning_encrypted_content"]); signature != "" {
				state.writeThinkingDelta(w, "signature_delta", "signature", signature)
			}
			if done, _ := delta["reasoning_done"].(bool); done && state.thinkIndex >= 0 {
				state.detachThinking(w)
			}
			if item, ok := delta["x_grok_search"].(map[string]interface{}); ok {
				done, _ := delta["x_grok_search_done"].(bool)
				state.writeSearch(w, item, done)
			}
			if content := streamString(delta["content"]); content != "" {
				state.writeText(w, content)
			}
			if refusal := streamString(delta["refusal"]); refusal != "" {
				state.writeText(w, refusal)
				state.sawRefusal = true
				if state.stopReason == "end_turn" {
					state.stopReason = "refusal"
				}
			}
			for _, call := range interfaceSlice(delta["tool_calls"]) {
				if err := state.writeToolCall(w, call); err != nil {
					return err
				}
			}
			state.writeCitations(w, interfaceSlice(delta["annotations"]))
			if stop := interfaceString(delta["stop_sequence"]); stop != "" {
				state.stopSequence = stop
			}
			if finish := interfaceString(choice["finish_reason"]); finish != "" && finish != "<nil>" {
				state.stopReason = openAIFinishToAnthropic(finish)
				terminal = true
			}
		}
		return tracked.Err
	})
	if err == io.EOF {
		err = nil
	}
	if err == nil && !terminal {
		err = fmt.Errorf("chat stream closed before completion")
	}
	if err != nil {
		if tracked.Err == nil {
			writeAnthropicSSE(w, "error", map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": "api_error", "message": err.Error()}})
		}
		return err
	}
	if state.stopSequence != "" {
		state.stopReason = "stop_sequence"
	}
	state.finish(w)
	return tracked.Err
}

func streamString(value interface{}) string {
	text, _ := value.(string)
	return text
}

func (s *anthropicStreamState) startBlock(w io.Writer, block map[string]interface{}) int {
	index := s.nextIndex
	s.nextIndex++
	s.open[index] = true
	writeAnthropicSSE(w, "content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": index, "content_block": block,
	})
	return index
}

func (s *anthropicStreamState) closeBlock(w io.Writer, index int) {
	if !s.open[index] {
		return
	}
	writeAnthropicSSE(w, "content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": index})
	delete(s.open, index)
}

func (s *anthropicStreamState) closeTextualBlocks(w io.Writer) {
	if s.textIndex >= 0 {
		s.closeBlock(w, s.textIndex)
		s.textIndex = -1
	}
	s.detachThinking(w)
}

// A named thinking block may receive its signature only in the final snapshot.
// Keep that block addressable until signed (or message end), so a late signature
// attaches to the original thinking instead of creating an empty extra block.
func (s *anthropicStreamState) detachThinking(w io.Writer) {
	if s.thinkIndex >= 0 {
		if s.reasoningID == "" || s.reasoningSigned[s.thinkIndex] {
			s.closeBlock(w, s.thinkIndex)
		}
		s.thinkIndex = -1
	}
}

func (s *anthropicStreamState) writeText(w io.Writer, text string) {
	s.detachThinking(w)
	if s.textIndex < 0 {
		s.textIndex = s.startBlock(w, map[string]interface{}{"type": "text", "text": ""})
	}
	writeAnthropicContentBlockDelta(w, s.textIndex, map[string]interface{}{"type": "text_delta", "text": text})
}

func (s *anthropicStreamState) writeThinkingDelta(w io.Writer, deltaType, field, value string) {
	if s.textIndex >= 0 && deltaType != "signature_delta" {
		s.closeBlock(w, s.textIndex)
		s.textIndex = -1
	}
	if s.thinkIndex < 0 {
		if index, exists := s.reasoningIndexes[s.reasoningID]; exists && s.reasoningID != "" {
			if !s.open[index] {
				return
			}
			s.thinkIndex = index
		} else {
			s.thinkIndex = s.startBlock(w, map[string]interface{}{"type": "thinking", "thinking": "", "signature": ""})
			if s.reasoningID != "" {
				s.reasoningIndexes[s.reasoningID] = s.thinkIndex
			}
		}
	}
	if deltaType == "signature_delta" {
		if s.reasoningSigned[s.thinkIndex] {
			return
		}
		s.reasoningSigned[s.thinkIndex] = true
	}
	writeAnthropicContentBlockDelta(w, s.thinkIndex, map[string]interface{}{"type": deltaType, field: value})
}

func (s *anthropicStreamState) writeToolCall(w io.Writer, raw interface{}) error {
	call, _ := raw.(map[string]interface{})
	callIndex := interfaceToInt(call["index"])
	fn, _ := call["function"].(map[string]interface{})
	blockIndex, exists := s.toolIndexes[callIndex]
	if !exists {
		s.closeTextualBlocks(w)
		id, name := interfaceString(call["id"]), interfaceString(fn["name"])
		if id == "" || id == "<nil>" || name == "" || name == "<nil>" {
			return fmt.Errorf("tool call start requires a valid id and function name")
		}
		if s.toolIDs == nil {
			s.toolIDs = map[string]int{}
		}
		if _, duplicate := s.toolIDs[id]; duplicate {
			return fmt.Errorf("duplicate tool call id %q", id)
		}
		s.toolIDs[id] = callIndex
		blockIndex = s.startBlock(w, map[string]interface{}{"type": "tool_use", "id": id, "name": name, "input": map[string]interface{}{}})
		s.toolIndexes[callIndex] = blockIndex
	}
	if args := streamString(fn["arguments"]); args != "" {
		writeAnthropicContentBlockDelta(w, blockIndex, map[string]interface{}{"type": "input_json_delta", "partial_json": args})
	}
	return nil
}

func (s *anthropicStreamState) finish(w io.Writer) {
	if len(s.searches) > 0 {
		s.usage["server_tool_use"] = map[string]interface{}{"web_search_requests": len(s.searches)}
	}
	// Closing every open block must happen in index order, and closeBlock mutates
	// s.open, so the keys are collected before the loop walks them.
	for _, index := range slices.Sorted(maps.Keys(s.open)) {
		s.closeBlock(w, index)
	}
	if s.stopReason == "" {
		if len(s.toolIndexes) > 0 {
			s.stopReason = "tool_use"
		} else {
			s.stopReason = "end_turn"
		}
	}
	writeAnthropicSSE(w, "message_delta", map[string]interface{}{
		"type": "message_delta", "delta": map[string]interface{}{"stop_reason": s.stopReason, "stop_sequence": nullableProtocolString(s.stopSequence)},
		"usage": s.usage,
	})
	writeAnthropicSSE(w, "message_stop", map[string]interface{}{"type": "message_stop"})
}

// writeAnthropicContentBlockDelta frames one content_block_delta event; every
// streamed text, thinking and tool-argument delta shares this envelope.
func writeAnthropicContentBlockDelta(w io.Writer, index int, delta map[string]interface{}) {
	writeAnthropicSSE(w, "content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": index, "delta": delta,
	})
}

func writeAnthropicSSE(w io.Writer, event string, payload interface{}) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeAnthropicModelNotFound(w http.ResponseWriter, model string) {
	util.WriteJSONStatus(w, http.StatusNotFound, map[string]interface{}{
		"type": "error", "error": map[string]interface{}{"type": "not_found_error", "message": modelNotFoundMessage(model)},
	})
}

// anthropicErrorStatuses pairs the statuses that have a dedicated Anthropic
// error identity with their (type, code). Both derivations used to walk the same
// status chain separately; the pair keeps them in sync.
var anthropicErrorStatuses = map[int][2]string{
	http.StatusUnauthorized:          {"authentication_error", "authentication_error"},
	http.StatusForbidden:             {"permission_error", "permission_error"},
	http.StatusNotFound:              {"not_found_error", "not_found_error"},
	http.StatusRequestEntityTooLarge: {"request_too_large", "request_too_large"},
	http.StatusTooManyRequests:       {"rate_limit_error", "rate_limit_error"},
	// Anthropic reports capacity and upstream trouble as overloaded_error, which
	// is the type its clients back off on.
	http.StatusServiceUnavailable: {"overloaded_error", "upstream_unavailable"},
	http.StatusBadGateway:         {"overloaded_error", "upstream_unavailable"},
	http.StatusGatewayTimeout:     {"overloaded_error", "upstream_unavailable"},
}

// anthropicErrorType derives the Anthropic error type from the HTTP status.
// A constant invalid_request_error made an exhausted allowance, an upstream
// overload and a bad request indistinguishable, and an Anthropic client decides
// whether to retry from that field.
func anthropicErrorType(status int) string {
	if pair, ok := anthropicErrorStatuses[status]; ok {
		return pair[0]
	}
	return "invalid_request_error"
}

// anthropicErrorCode is the stable machine code paired with the type above.
func anthropicErrorCode(status int) string {
	if pair, ok := anthropicErrorStatuses[status]; ok {
		return pair[1]
	}
	if status >= 500 {
		return "upstream_unavailable"
	}
	return "invalid_request"
}

func writeAnthropicError(w http.ResponseWriter, status int, message string) {
	util.WriteJSONStatus(w, status, map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    anthropicErrorType(status),
			"code":    anthropicErrorCode(status),
			"message": message,
		},
	})
}

func writeAnthropicUpstreamError(w http.ResponseWriter, status int, body string) {
	if status < 400 {
		status = http.StatusBadGateway
	}
	// The raw body is the upstream's own words: it names the account, the team
	// and the refusal reason. It belongs in the logs, never in the client's
	// error envelope.
	message := apperrors.PublicMessage(body)
	if strings.TrimSpace(message) == "" {
		message = http.StatusText(status)
	}
	writeAnthropicError(w, status, message)
}

// isAnthropicInstructionRole reports whether a message-level role carries
// instructions rather than conversation turns. Anthropic has no such role in
// `messages`; the text belongs in the system prompt.
func isAnthropicInstructionRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer":
		return true
	default:
		return false
	}
}
