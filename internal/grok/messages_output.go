package grok

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
	"slices"
	"strings"
	"time"

	"encoding/json"

	apperrors "orchids-api/internal/errors"
)

func anthropicResponseFromChat(model string, chat map[string]any) map[string]any {
	content := make([]any, 0)
	stopReason := "end_turn"
	sawRefusal := false
	stopSequence := ""
	choices, _ := chat["choices"].([]any)
	if len(choices) > 0 {
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		stopSequence = chatwire.ParseLooseStringAny(message["stop_sequence"])
		thinking := strings.TrimSpace(fmt.Sprint(responses.FirstNonNil(message["reasoning_content"], message["reasoning"])))
		signature := strings.TrimSpace(fmt.Sprint(message["reasoning_encrypted_content"]))
		if items := responses.InterfaceSlice(message["x_grok_reasoning"]); len(items) > 0 {
			for _, item := range items {
				wrapper := map[string]any{"output": []any{item}}
				content = append(content, map[string]any{"type": "thinking", "thinking": consoleExtractReasoningText(wrapper), "signature": consoleExtractEncryptedReasoning(wrapper)})
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
			block := map[string]any{"type": "thinking", "thinking": thinking}
			if signature != "" {
				block["signature"] = signature
			}
			content = append(content, block)
		}
		for _, raw := range responses.InterfaceSlice(message["x_grok_searches"]) {
			item, _ := raw.(map[string]any)
			content = append(content, searchContent(item)...)
		}
		if text, ok := message["content"].(string); ok && text != "" {
			block := map[string]any{"type": "text", "text": text}
			if citations := chatCitations(responses.InterfaceSlice(message["annotations"])); len(citations) > 0 {
				block["citations"] = citations
			}
			content = append(content, block)
		}
		toolCalls, _ := message["tool_calls"].([]any)
		if refusal := responses.StreamString(message["refusal"]); refusal != "" {
			content = append(content, map[string]any{"type": "text", "text": refusal})
			sawRefusal = true
		}
		for _, raw := range toolCalls {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			input := any(map[string]any{})
			switch args := fn["arguments"].(type) {
			case string:
				_ = json.Unmarshal([]byte(args), &input)
			case nil:
			default:
				input = args
			}
			content = append(content, map[string]any{
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
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	usage, _ := chat["usage"].(map[string]any)
	return map[string]any{
		// The id the Chat Completions layer produced is a chatcmpl_* value; the
		// Anthropic contract uses msg_*. Handing the caller a chatcmpl_ message
		// id broke every client that validates the prefix and lost the upstream
		// identity, so an Anthropic-shaped id is generated instead.
		"id":   anthropicMessageID(chatwire.ParseLooseStringAny(chat["id"])),
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
		return "msg_" + util.RandomHex(12)
	}
	if after, ok := strings.CutPrefix(trimmed, "chatcmpl_"); ok {
		return "msg_" + after
	}
	return trimmed
}

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

func anthropicUsageFromOpenAI(usage map[string]any) map[string]any {
	input := max(0, responses.InterfaceToInt(usage["prompt_tokens"]))
	cached := 0
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		cached = max(0, responses.InterfaceToInt(details["cached_tokens"]))
	}
	cached = min(cached, input)
	output := max(0, responses.InterfaceToInt(usage["completion_tokens"]))
	result := map[string]any{
		"input_tokens": input - cached,
		// Anthropic reports both cache counters unconditionally, with 0 meaning
		// "no cache". Omitting them made a caller unable to tell "no cache" from
		// "field missing" (Claude Code's /cost and cache statistics read them).
		"cache_read_input_tokens":     cached,
		"cache_creation_input_tokens": 0,
		"output_tokens":               output,
	}
	reasoning := 0
	if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
		reasoning = max(0, responses.InterfaceToInt(details["reasoning_tokens"]))
	}
	result["output_tokens_details"] = map[string]any{
		"thinking_tokens": min(output, reasoning),
	}
	if cost := responses.InterfaceToInt(usage["cost_in_usd_ticks"]); cost > 0 {
		result["cost_in_usd_ticks"] = cost
	}
	if sources := responses.InterfaceToInt(responses.FirstNonNil(usage["num_sources_used"], usage["num_server_side_tools_used"])); sources > 0 {
		result["num_sources_used"] = sources
	}
	return result
}

func (h *Handler) serveAnthropicMessageStream(w http.ResponseWriter, req *http.Request, model string, inputTokens int) {
	h.withChatStream(req, func(status int, header http.Header, reader io.Reader) {
		maps.Copy(w.Header(), header)
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
	usage            map[string]any
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
	if target, ok := w.(http.ResponseWriter); ok {
		w = httpserver.DeadlineResponseWriter{ResponseWriter: target}
	}
	tracked := &checkedStreamWriter{Target: w}
	w = tracked
	state := &anthropicStreamState{
		id: "msg_" + util.RandomHex(12), model: model, textIndex: -1, thinkIndex: -1,
		toolIndexes: map[int]int{}, open: map[int]bool{},
		// Named thinking blocks stay addressable by their upstream id until they
		// are signed, so the signature of a late snapshot attaches to the original
		// block instead of an empty extra one.
		reasoningIndexes: map[string]int{}, reasoningSigned: map[int]bool{},
		// Upstream usage arrives on the terminal chunk, after message_start. The
		// request is already parsed here, so seed Anthropic's required input/cache
		// counters from the same conservative estimator used by chat fallback.
		usage:    anthropicUsageFromOpenAI(map[string]any{"prompt_tokens": max(0, inputTokens), "completion_tokens": 0}),
		searches: map[string]*messageSearchState{}, citations: map[string]bool{},
	}
	writeAnthropicSSE(w, "message_start", map[string]any{
		"type": "message_start", "message": map[string]any{"id": state.id, "type": "message", "role": "assistant", "model": model, "created_at": time.Now().Unix(),
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": state.usage},
	})
	if tracked.Err != nil {
		return tracked.Err
	}
	terminal := false
	err := responses.ReadSSEBytes(reader, func(event string, data []byte) error {
		if tracked.Err != nil {
			return tracked.Err
		}
		if string(data) == "[DONE]" {
			if !terminal {
				return fmt.Errorf("chat stream ended without finish_reason")
			}
			return io.EOF
		}
		var chunk map[string]any
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("invalid chat SSE: %w", err)
		}
		if chunk["error"] != nil || event == "error" {
			return responses.Failure(chunk)
		}
		if usage, ok := chunk["usage"].(map[string]any); ok {
			state.usage = anthropicUsageFromOpenAI(usage)
		}
		for _, rawChoice := range responses.InterfaceSlice(chunk["choices"]) {
			choice, _ := rawChoice.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if len(delta) == 0 {
				delta, _ = choice["message"].(map[string]any)
			}
			if key := chatwire.ParseLooseStringAny(delta["reasoning_item_id"]); key != "" && key != state.reasoningID {
				state.detachThinking(w)
				state.reasoningID = key
			}
			if thinking := responses.StreamString(responses.FirstNonNil(delta["reasoning_content"], delta["reasoning"])); thinking != "" {
				state.writeThinkingDelta(w, "thinking_delta", "thinking", thinking)
			}
			if signature := responses.StreamString(delta["reasoning_encrypted_content"]); signature != "" {
				state.writeThinkingDelta(w, "signature_delta", "signature", signature)
			}
			if done, _ := delta["reasoning_done"].(bool); done && state.thinkIndex >= 0 {
				state.detachThinking(w)
			}
			if item, ok := delta["x_grok_search"].(map[string]any); ok {
				done, _ := delta["x_grok_search_done"].(bool)
				state.writeSearch(w, item, done)
			}
			if content := responses.StreamString(delta["content"]); content != "" {
				state.writeText(w, content)
			}
			if refusal := responses.StreamString(delta["refusal"]); refusal != "" {
				state.writeText(w, refusal)
				state.sawRefusal = true
				if state.stopReason == "end_turn" {
					state.stopReason = "refusal"
				}
			}
			for _, call := range responses.InterfaceSlice(delta["tool_calls"]) {
				if err := state.writeToolCall(w, call); err != nil {
					return err
				}
			}
			state.writeCitations(w, responses.InterfaceSlice(delta["annotations"]))
			if stop := chatwire.ParseLooseStringAny(delta["stop_sequence"]); stop != "" {
				state.stopSequence = stop
			}
			if finish := chatwire.ParseLooseStringAny(choice["finish_reason"]); finish != "" && finish != "<nil>" {
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
			writeAnthropicSSE(w, "error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
		}
		return err
	}
	if state.stopSequence != "" {
		state.stopReason = "stop_sequence"
	}
	state.finish(w)
	return tracked.Err
}

func (s *anthropicStreamState) startBlock(w io.Writer, block map[string]any) int {
	index := s.nextIndex
	s.nextIndex++
	s.open[index] = true
	writeAnthropicSSE(w, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": block,
	})
	return index
}

func (s *anthropicStreamState) closeBlock(w io.Writer, index int) {
	if !s.open[index] {
		return
	}
	writeAnthropicSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
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
		s.textIndex = s.startBlock(w, map[string]any{"type": "text", "text": ""})
	}
	writeAnthropicContentBlockDelta(w, s.textIndex, map[string]any{"type": "text_delta", "text": text})
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
			s.thinkIndex = s.startBlock(w, map[string]any{"type": "thinking", "thinking": "", "signature": ""})
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
	writeAnthropicContentBlockDelta(w, s.thinkIndex, map[string]any{"type": deltaType, field: value})
}

func (s *anthropicStreamState) writeToolCall(w io.Writer, raw any) error {
	call, _ := raw.(map[string]any)
	callIndex := responses.InterfaceToInt(call["index"])
	fn, _ := call["function"].(map[string]any)
	blockIndex, exists := s.toolIndexes[callIndex]
	if !exists {
		s.closeTextualBlocks(w)
		id, name := chatwire.ParseLooseStringAny(call["id"]), chatwire.ParseLooseStringAny(fn["name"])
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
		blockIndex = s.startBlock(w, map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
		s.toolIndexes[callIndex] = blockIndex
	}
	if args := responses.StreamString(fn["arguments"]); args != "" {
		writeAnthropicContentBlockDelta(w, blockIndex, map[string]any{"type": "input_json_delta", "partial_json": args})
	}
	return nil
}

func (s *anthropicStreamState) finish(w io.Writer) {
	if len(s.searches) > 0 {
		s.usage["server_tool_use"] = map[string]any{"web_search_requests": len(s.searches)}
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
	writeAnthropicSSE(w, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": s.stopReason, "stop_sequence": nullableProtocolString(s.stopSequence)},
		"usage": s.usage,
	})
	writeAnthropicSSE(w, "message_stop", map[string]any{"type": "message_stop"})
}

// writeAnthropicContentBlockDelta frames one content_block_delta event; every
// streamed text, thinking and tool-argument delta shares this envelope.
func writeAnthropicContentBlockDelta(w io.Writer, index int, delta map[string]any) {
	writeAnthropicSSE(w, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index, "delta": delta,
	})
}

func writeAnthropicSSE(w io.Writer, event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if tracked, ok := w.(*checkedStreamWriter); ok {
		_ = tracked.WriteSSE(event, raw)
	} else {
		_ = httpserver.WriteSSEFrame(w, event, raw)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeAnthropicModelNotFound(w http.ResponseWriter, model string) {
	util.WriteJSONStatus(w, http.StatusNotFound, map[string]any{
		"type": "error", "error": map[string]any{"type": "not_found_error", "message": modelNotFoundMessage(model)},
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
	util.WriteJSONStatus(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
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
