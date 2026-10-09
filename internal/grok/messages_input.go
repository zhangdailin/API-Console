package grok

import (
	"fmt"
	"maps"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"slices"
	"strings"
)

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
				if encrypted := chatwire.ParseLooseStringAny(responses.FirstNonNil(block["signature"], block["data"])); encrypted != "" {
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
			if signature := strings.TrimSpace(fmt.Sprint(responses.FirstNonNil(block["signature"], block["data"]))); signature != "" && signature != "<nil>" {
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
