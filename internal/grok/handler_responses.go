package grok

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"strings"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/util"
)

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func newCaptureResponseWriter() *captureResponseWriter {
	return &captureResponseWriter{header: make(http.Header), code: http.StatusOK}
}

func (w *captureResponseWriter) Header() http.Header { return w.header }

func (w *captureResponseWriter) WriteHeader(code int) {
	if code != 0 {
		w.code = code
	}
}

func (w *captureResponseWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

func (w *captureResponseWriter) Flush() {}

func (h *Handler) HandleResponses(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	// Keep the original object for the Build CLI route.  The CLI upstream
	// speaks Responses natively, so translating it through Chat Completions
	// would drop valid fields such as previous_response_id and metadata.
	body, err := readBoundedJSONBody(w, r)
	if err != nil {
		return
	}
	var req responses.CreateRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeGrokError(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Model = normalizeModelID(req.Model)
	// Publish the resolved model so the per-minute aggregation can attribute this
	// request to a model; the latency middleware never re-reads the body.
	r = r.WithContext(middleware.WithRequestModel(r.Context(), req.Model))
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	h.applyDefaultResponsesStream(&req)
	var nativePayload map[string]interface{}
	if err := json.Unmarshal(body, &nativePayload); err != nil || nativePayload == nil {
		writeGrokError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if _, provided := nativePayload["stream"]; !provided {
		nativePayload["stream"] = req.Stream
	}
	identityMessages, _ := responsesInputToMessagesMode(req.Input, false)
	session := prepareGrokSession(r, req.Model, req.PromptCacheKey, identityMessages)
	if session.Key != "" {
		nativePayload["prompt_cache_key"] = session.Key
		r = r.WithContext(withGrokSession(r.Context(), session))
		if session.Replay {
			h.applyNativeReasoningReplay(req.Model, session.Key, nativePayload)
		}
	}

	spec, resolved := h.resolveConversationModel(r.Context(), req.Model)
	if !resolved {
		writeResponsesAPIError(w, http.StatusBadRequest, "invalid_request_error", modelNotFoundMessage(req.Model))
		return
	}
	// Every resolvable Grok model uses Build CLI, so the native Responses path
	// handles all of them; other providers use ResponsesChannelBridgeHandler.
	// A compaction turn is not a conversation turn: Codex remote-v2 sends
	// `compaction_trigger` and the Grok TUI appends the canonical summary
	// prompt as its last user item. The gateway answers those itself so the
	// resulting state stays portable across accounts.
	if h.GatewayCompactionEnabled() && responses.ClassifyCompactionPayload(nativePayload) != responses.CompactionNone {
		h.handleGatewayCompaction(w, r, req.Model, spec, nativePayload, responsesPayloadStreaming(nativePayload, req.Stream))
		return
	}
	h.handleNativeCLIResponses(w, r, req.Model, spec, nativePayload)
}

func expandStoredResponseInput(responseBody []byte, current interface{}) (interface{}, error) {
	var previous map[string]interface{}
	if json.Unmarshal(responseBody, &previous) != nil {
		return nil, fmt.Errorf("stored response is invalid")
	}
	output := interfaceSlice(previous["output"])
	if len(output) == 0 {
		return nil, fmt.Errorf("stored response has no output")
	}
	currentItems := make([]interface{}, 0)
	switch value := current.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			currentItems = append(currentItems, map[string]interface{}{
				"type": "message", "role": "user", "content": []interface{}{map[string]interface{}{"type": "input_text", "text": value}},
			})
		}
	case []interface{}:
		currentItems = append(currentItems, value...)
	default:
		return nil, fmt.Errorf("input must be a string or an array")
	}
	if len(currentItems) == 0 {
		return nil, fmt.Errorf("input is required")
	}
	combined := make([]interface{}, 0, len(output)+len(currentItems))
	combined = append(combined, output...)
	combined = append(combined, currentItems...)
	return combined, nil
}

// handleNativeCLIResponses proxies Build OAuth Responses requests without a
// Chat-Completions compatibility conversion.  Besides preserving the official
// request and event schema, this keeps SSE streaming realtime and bounded by
// the normal HTTP backpressure instead of buffering the whole completion.
func (h *Handler) handleNativeCLIResponses(w http.ResponseWriter, r *http.Request, modelID string, spec ModelSpec, payload map[string]interface{}) {
	h.handleNativeCLIResponsesAt(w, r, modelID, spec, payload, "/responses", true)
}

func copyNativeCLIResponseHeaders(dst, src http.Header) {
	// Forward only end-to-end response metadata. Hop-by-hop headers must not be
	// copied because net/http owns the downstream connection.
	for _, key := range []string{"Content-Type", "Cache-Control", "X-Request-Id", "X-Request-ID", "X-Grok2api-Compatibility-Warnings", "X-Grok2api-Reasoning-Recovery"} {
		if values, ok := src[key]; ok {
			dst.Del(key)
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}

func streamNativeCLIResponse(w http.ResponseWriter, body io.Reader) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}

// validateResponsesCompatibilityFor validates a Responses request.
// allowStreamedStore is true for a provider that can persist a streamed
// response (the chat bridge when a response store is configured); Grok's native
// path cannot, so it keeps rejecting store=true with stream=true.
func validateResponsesCompatibilityFor(req responses.CreateRequest, allowStreamedStore bool) error {
	if req.Store != nil && *req.Store && req.Stream && !allowStreamedStore {
		return fmt.Errorf("store=true requires stream=false for this provider")
	}
	if truncation := strings.ToLower(strings.TrimSpace(req.Truncation)); truncation != "" && truncation != "auto" && truncation != "disabled" {
		return fmt.Errorf("truncation must be auto or disabled")
	}
	if req.Background != nil && *req.Background {
		return fmt.Errorf("background=true is not supported")
	}
	return nil
}

func (h *Handler) applyDefaultResponsesStream(req *responses.CreateRequest) {
	if req == nil || req.StreamProvided {
		return
	}
	req.Stream = h.defaultChatStream()
}

func chatRequestFromResponses(req responses.CreateRequest) (chatwire.Request, error) {
	model := normalizeModelID(req.Model)
	if strings.TrimSpace(model) == "" {
		return chatwire.Request{}, fmt.Errorf("model is required")
	}
	messages, err := responsesInputToMessages(req.Input)
	if err != nil {
		return chatwire.Request{}, err
	}
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		messages = append([]chatwire.Message{{Role: "system", Content: instructions}}, messages...)
	}
	reasoningEffort := responsesReasoningEffort(req.Reasoning)
	// Hosted tools (web_search / x_search) are the server-side searches the
	// caller switched on. They have no function object, so they travel in
	// ResponsesTools — the same slot the Anthropic bridge uses for its server
	// tools — and the native planes forward them to the upstream. Keeping only
	// the function declarations meant an explicit search request arrived as a
	// plain text turn, and the model then answered that it had no web access.
	tools, hostedTools := responsesToolsToChatTools(req.Tools)
	out := chatwire.Request{
		SourceOperation:   "responses",
		Model:             model,
		Messages:          messages,
		Stream:            req.Stream,
		StreamProvided:    true,
		ReasoningEffort:   reasoningEffort,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		Tools:             tools,
		ResponsesTools:    hostedTools,
		ToolChoice:        responsesToolChoiceToChat(req.ToolChoice),
		ParallelToolCalls: req.ParallelToolCalls,
		MaxTokens:         req.MaxOutputTokens,
		PromptCacheKey:    req.PromptCacheKey,
		Metadata:          cloneStringInterfaceMap(req.Metadata),
		// Include and the output format used to be dropped here while the native
		// Grok path forwarded them, so the same request produced reasoning
		// summaries and structured output on one channel and neither on the
		// others. They are carried through instead of re-derived: the chat layer
		// already knows which of them its upstream honors.
		Include:        append([]string(nil), req.Include...),
		ResponseText:   responsesTextControls(req),
		ResponseFormat: responsesOutputFormat(req),
	}
	return out, nil
}

// responsesTextControls reports the `text` object to hand to the chat layer.
//
// Responses spells the output format as text.format while chat spells it as
// response_format. Both are forwarded unchanged — the chat layer normalizes the
// shape it receives — so a bridge that receives either one keeps the caller's
// intent, including the `text.verbosity` control the format object may carry.
func responsesTextControls(req responses.CreateRequest) map[string]interface{} {
	return cloneStringInterfaceMap(req.Text)
}

// responsesOutputFormat derives the chat `response_format` from either spelling.
// `text.format` wins when both are present because it is the field the Responses
// API defines.
func responsesOutputFormat(req responses.CreateRequest) map[string]interface{} {
	if format, ok := req.Text["format"].(map[string]interface{}); ok && len(format) > 0 {
		return cloneStringInterfaceMap(format)
	}
	return cloneStringInterfaceMap(req.ResponseFormat)
}

func responsesInputToMessages(input interface{}) ([]chatwire.Message, error) {
	return responsesInputToMessagesMode(input, true)
}

// Native identity extraction never changes the payload sent upstream. Unknown
// items remain in that original payload; only the chat bridge rejects them.
func responsesInputToMessagesMode(input interface{}, strict bool) ([]chatwire.Message, error) {
	switch v := input.(type) {
	case nil:
		return nil, fmt.Errorf("input is required")
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("input is required")
		}
		return []chatwire.Message{{Role: "user", Content: v}}, nil
	case []interface{}:
		messages := make([]chatwire.Message, 0, len(v))
		for index, raw := range v {
			item, _ := raw.(map[string]interface{})
			if item == nil {
				if !strict {
					continue
				}
				return nil, fmt.Errorf("input[%d] must be an object", index)
			}
			itemType := strings.ToLower(strings.TrimSpace(chatwire.ParseLooseStringAny(item["type"])))
			if itemType == "" {
				if strings.TrimSpace(chatwire.ParseLooseStringAny(item["role"])) != "" {
					itemType = "message"
				}
			}
			switch itemType {
			case "function_call":
				name := chatwire.ParseLooseStringAny(item["name"])
				if name == "" {
					if !strict {
						continue
					}
					return nil, fmt.Errorf("input[%d].name is required", index)
				}
				if strict && chatwire.ParseLooseStringAny(item["call_id"]) == "" {
					return nil, fmt.Errorf("input[%d].call_id is required", index)
				}
				args := "{}"
				if rawArgs := item["arguments"]; rawArgs != nil {
					switch x := rawArgs.(type) {
					case string:
						if strings.TrimSpace(x) != "" {
							args = strings.TrimSpace(x)
						}
					default:
						if buf, err := json.Marshal(x); err == nil {
							args = string(buf)
						}
					}
				}
				messages = append(messages, chatwire.Message{
					Role:    "assistant",
					Content: nil,
					ToolCalls: []chatwire.ToolCall{{
						ID:   strings.TrimSpace(fmt.Sprint(item["call_id"])),
						Type: "function",
						Function: map[string]interface{}{
							"name":      name,
							"arguments": args,
						},
					}},
				})
			case "function_call_output":
				if strict && chatwire.ParseLooseStringAny(item["call_id"]) == "" {
					return nil, fmt.Errorf("input[%d].call_id is required", index)
				}
				messages = append(messages, chatwire.Message{
					Role:       "tool",
					ToolCallID: strings.TrimSpace(fmt.Sprint(item["call_id"])),
					Content:    bridgeToolOutput(item["output"]),
				})
			case "custom_tool_call":
				name := firstNonEmpty(chatwire.ParseLooseStringAny(item["name"]), "custom_tool")
				if strict && chatwire.ParseLooseStringAny(item["call_id"]) == "" {
					return nil, fmt.Errorf("input[%d].call_id is required", index)
				}
				arguments, _ := json.Marshal(map[string]interface{}{"input": firstNonNil(item["input"], item["arguments"], "")})
				messages = append(messages, chatwire.Message{Role: "assistant", Content: nil, ToolCalls: []chatwire.ToolCall{{
					ID: firstNonEmpty(chatwire.ParseLooseStringAny(item["call_id"]), chatwire.ParseLooseStringAny(item["id"])), Type: "function",
					Function: map[string]interface{}{"name": name, "arguments": string(arguments)},
				}}})
			case "custom_tool_call_output":
				if strict && chatwire.ParseLooseStringAny(item["call_id"]) == "" {
					return nil, fmt.Errorf("input[%d].call_id is required", index)
				}
				messages = append(messages, chatwire.Message{Role: "tool", ToolCallID: chatwire.ParseLooseStringAny(item["call_id"]), Content: bridgeToolOutput(item["output"])})
			case "reasoning":
				if strict && chatwire.ParseLooseStringAny(item["encrypted_content"]) != "" {
					return nil, fmt.Errorf("input[%d]: encrypted reasoning requires a native Responses provider", index)
				}
				messages = append(messages, chatwire.Message{
					Role: "assistant", Content: "",
					ReasoningContent: responsesReasoningSummary(item), ReasoningEncryptedContent: chatwire.ParseLooseStringAny(item["encrypted_content"]),
				})
			case "message":
				role := chatwire.ParseLooseStringAny(item["role"])
				if role == "" {
					role = "user"
				}
				messages = append(messages, chatwire.Message{
					Role:    role,
					Content: normalizeResponsesMessageContent(item["content"]),
				})
			default:
				if !strict {
					continue
				}
				return nil, fmt.Errorf("input[%d]: unsupported Responses item type %q", index, itemType)
			}
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("input is required")
		}
		return messages, nil
	default:
		return nil, fmt.Errorf("input must be a string or an array")
	}
}

func bridgeToolOutput(output interface{}) string {
	if text, ok := output.(string); ok {
		return text
	}
	if output == nil {
		return ""
	}
	encoded, _ := json.Marshal(output)
	return string(encoded)
}

func responsesReasoningSummary(item map[string]interface{}) string {
	parts := make([]string, 0)
	for _, raw := range interfaceSlice(item["summary"]) {
		part, _ := raw.(map[string]interface{})
		if text := chatwire.ParseLooseStringAny(part["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func normalizeResponsesMessageContent(content interface{}) interface{} {
	parts, ok := content.([]interface{})
	if !ok {
		return content
	}
	out := make([]interface{}, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]interface{})
		if part == nil {
			continue
		}
		ptype := strings.ToLower(strings.TrimSpace(fmt.Sprint(part["type"])))
		switch ptype {
		case "input_text", "output_text":
			out = append(out, map[string]interface{}{"type": "text", "text": fmt.Sprint(part["text"])})
		case "input_image", "image":
			if url := responsesPartURL(part, []string{"image_url", "source"}, []string{"url"}); url != "" {
				out = append(out, map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}})
			}
		case "input_file", "file":
			url := responsesPartURL(part, []string{"file", "file_url", "source", "file_data"}, []string{"url", "file_url", "data", "file_data"})
			if url == "" {
				url = chatwire.ParseLooseStringAny(part["file_id"])
			}
			if url != "" {
				// The chat layer validates the portable file shape, so carry the
				// resolved reference as file_data instead of a nested url. This
				// also makes the chat->Responses->chat round trip lossless.
				out = append(out, map[string]interface{}{"type": "file", "file": map[string]interface{}{"file_data": url}})
			}
		default:
			out = append(out, part)
		}
	}
	return out
}

func responsesPartURL(part map[string]interface{}, keys, nestedKeys []string) string {
	for _, key := range keys {
		raw := part[key]
		switch v := raw.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]interface{}:
			for _, nestedKey := range nestedKeys {
				if s := chatwire.ParseLooseStringAny(v[nestedKey]); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

// responsesToolsToChatTools splits a Responses tool list into the function
// declarations the Chat layer emulates and the hosted (server-side) tools it
// forwards untouched. A hosted tool has no `function` object, so it has no
// ToolDef representation: dropping it was how web_search and x_search silently
// disappeared between the Responses endpoint and the upstream.
func responsesToolsToChatTools(tools []map[string]interface{}) ([]chatwire.ToolDef, []map[string]interface{}) {
	functions := make([]chatwire.ToolDef, 0, len(tools))
	var hosted []map[string]interface{}
	seenHosted := map[string]struct{}{}
	for _, tool := range tools {
		declaredType := strings.TrimSpace(fmt.Sprint(tool["type"]))
		if !strings.EqualFold(declaredType, "function") {
			if normalized, native := chatwire.NativeToolTypes[strings.ToLower(declaredType)]; native {
				// The hosted list is forwarded verbatim, so it carries the same
				// uniqueness contract the function list is validated for: a
				// duplicate would reach the upstream as two identical searches.
				if _, duplicate := seenHosted[normalized]; duplicate {
					continue
				}
				seenHosted[normalized] = struct{}{}
				hosted = append(hosted, cloneStringInterfaceMap(tool))
			}
			continue
		}
		if fn, _ := tool["function"].(map[string]interface{}); fn != nil {
			if strings.TrimSpace(fmt.Sprint(fn["name"])) != "" {
				functions = append(functions, chatwire.ToolDef{Type: "function", Function: fn})
			}
			continue
		}
		name := chatwire.ParseLooseStringAny(tool["name"])
		if name == "" {
			continue
		}
		functions = append(functions, chatwire.ToolDef{Type: "function", Function: map[string]interface{}{
			"name":        name,
			"description": strings.TrimSpace(fmt.Sprint(tool["description"])),
			"parameters":  firstNonNil(tool["parameters"], map[string]interface{}{}),
		}})
	}
	return functions, hosted
}

func responsesToolChoiceToChat(choice interface{}) interface{} {
	if choice == nil {
		return nil
	}
	m, _ := choice.(map[string]interface{})
	if m == nil || !strings.EqualFold(strings.TrimSpace(fmt.Sprint(m["type"])), "function") {
		return choice
	}
	if _, ok := m["function"].(map[string]interface{}); ok {
		return choice
	}
	name := chatwire.ParseLooseStringAny(m["name"])
	if name == "" {
		return choice
	}
	return map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": name}}
}

func responsesReasoningEffort(reasoning map[string]interface{}) *string {
	if len(reasoning) == 0 {
		return nil
	}
	if effort := strings.ToLower(strings.TrimSpace(fmt.Sprint(reasoning["effort"]))); effort != "" && effort != "<nil>" {
		return &effort
	}
	return nil
}

func firstNonNil(values ...interface{}) interface{} {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func responsesObjectFromChat(model string, chat map[string]interface{}) map[string]interface{} {
	output := responsesOutputFromChat(chat)
	finish := ""
	if choices := interfaceSlice(chat["choices"]); len(choices) > 0 {
		choice, _ := choices[0].(map[string]interface{})
		finish = streamString(choice["finish_reason"])
	}
	status, details := responses.StatusFromFinish(finish)
	result := map[string]interface{}{"id": "resp_" + randomHex(12), "object": "response", "created_at": time.Now().Unix(), "status": status, "model": firstNonEmpty(interfaceString(chat["model"]), model), "output": output, "parallel_tool_calls": true, "tool_choice": "auto", "usage": responsesUsageFromChat(chat["usage"])}
	if details != nil {
		result["incomplete_details"] = details
	}
	if chat["error"] != nil {
		result["status"] = "failed"
		result["error"] = chat["error"]
	}
	for _, raw := range output {
		item, _ := raw.(map[string]interface{})
		if item["type"] != "web_search_call" {
			item["status"] = status
		}
	}
	return result
}

func responsesOutputFromChat(chat map[string]interface{}) []interface{} {
	out := []interface{}{}
	choices := interfaceSlice(chat["choices"])
	if len(choices) == 0 {
		return out
	}
	choice, _ := choices[0].(map[string]interface{})
	message, _ := choice["message"].(map[string]interface{})
	if message == nil {
		return out
	}
	if thoughts := interfaceSlice(message["x_grok_reasoning"]); len(thoughts) > 0 {
		for _, raw := range thoughts {
			item, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			copy := cloneStringInterfaceMap(item)
			copy["id"] = "rs_" + randomHex(12)
			copy["status"] = "completed"
			out = append(out, copy)
		}
	} else {
		text := streamString(firstNonNil(message["reasoning_content"], message["reasoning"]))
		signature := streamString(message["reasoning_encrypted_content"])
		if text != "" || signature != "" {
			item := map[string]interface{}{"id": "rs_" + randomHex(12), "type": "reasoning", "status": "completed", "summary": []interface{}{}}
			if text != "" {
				item["summary"] = []interface{}{map[string]interface{}{"type": "summary_text", "text": text}}
			}
			if signature != "" {
				item["encrypted_content"] = signature
			}
			out = append(out, item)
		}
	}
	seen := map[string]bool{}
	for _, raw := range interfaceSlice(message["x_grok_searches"]) {
		search, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		key := searchIdentity(search)
		if seen[key] {
			continue
		}
		seen[key] = true
		item := cloneStringInterfaceMap(search)
		if interfaceString(item["id"]) == "" {
			item["id"] = "ws_" + randomHex(12)
		}
		out = append(out, item)
	}
	for _, raw := range interfaceSlice(message["tool_calls"]) {
		call, _ := raw.(map[string]interface{})
		if item := responseFunctionCallItem(call); item != nil {
			out = append(out, item)
		}
	}
	parts := []interface{}{}
	if text := streamString(message["content"]); text != "" {
		parts = append(parts, map[string]interface{}{"type": "output_text", "text": text, "annotations": responses.Annotations(message["annotations"])})
	}
	if text := streamString(message["refusal"]); text != "" {
		parts = append(parts, map[string]interface{}{"type": "refusal", "refusal": text})
	}
	if len(parts) > 0 {
		out = append(out, map[string]interface{}{"id": "msg_" + randomHex(12), "type": "message", "status": "completed", "role": "assistant", "content": parts})
	}
	return out
}

func responseFunctionCallItem(call map[string]interface{}) map[string]interface{} {
	if call == nil {
		return nil
	}
	fn, _ := call["function"].(map[string]interface{})
	name := chatwire.ParseLooseStringAny(fn["name"])
	if name == "" {
		return nil
	}
	args := streamString(fn["arguments"])
	args = util.FirstNonEmptyUntrimmed(args, "{}")
	return map[string]interface{}{
		"id":        "fc_" + randomHex(12),
		"type":      "function_call",
		"call_id":   firstNonEmpty(streamString(call["id"]), "call_"+randomHex(12)),
		"name":      name,
		"arguments": args,
		"status":    "completed",
	}
}

func responsesUsageFromChat(raw interface{}) map[string]interface{} {
	usage, _ := raw.(map[string]interface{})
	input := interfaceToInt(firstNonNil(usage["prompt_tokens"], usage["input_tokens"]))
	output := interfaceToInt(firstNonNil(usage["completion_tokens"], usage["output_tokens"]))
	total := interfaceToInt(usage["total_tokens"])
	if total == 0 {
		total = input + output
	}
	result := cloneStringInterfaceMap(usage)
	if result == nil {
		result = map[string]interface{}{}
	}
	delete(result, "prompt_tokens")
	delete(result, "completion_tokens")
	delete(result, "prompt_tokens_details")
	delete(result, "completion_tokens_details")
	result["input_tokens"] = input
	result["output_tokens"] = output
	result["total_tokens"] = total
	for target, source := range map[string]string{"input_tokens_details": "prompt_tokens_details", "output_tokens_details": "completion_tokens_details"} {
		if details, ok := firstNonNil(usage[target], usage[source]).(map[string]interface{}); ok {
			result[target] = cloneStringInterfaceMap(details)
		}
	}
	return result
}

func copyCapturedResponse(w http.ResponseWriter, rec *captureResponseWriter) {
	for k, values := range rec.Header() {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(rec.code)
	_, _ = w.Write(rec.body.Bytes())
}
