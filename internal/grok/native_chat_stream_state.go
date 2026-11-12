package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
)

// buildChatStream is the state of one Build Responses stream relayed as Chat
// Completions chunks.
type buildChatStream struct {
	h       *Handler
	req     *chatwire.Request
	outcome *chatOutcome

	output     io.Writer
	respWriter http.ResponseWriter
	stream     *httpserver.CheckedStreamWriter
	flusher    http.Flusher

	id       string
	created  int64
	terminal bool
	finishAs string

	text, reasoning, refusal strings.Builder
	sawText                  bool
	filter                   stopFilter
	annotations              []map[string]any

	tools   map[string]*responseToolState
	byCall  map[string]*responseToolState
	byIndex map[int]*responseToolState
	ordered []*responseToolState

	thoughts        map[string]*responseReasoningState
	activeReasoning string
	lastSignature   string
	replayItems     []any
	searchDone      map[string]bool
}

func newBuildChatStream(h *Handler, w http.ResponseWriter, req *chatwire.Request, outcome *chatOutcome) *buildChatStream {
	s := &buildChatStream{
		h: h, req: req, outcome: outcome,
		id: "chatcmpl_" + util.RandomHex(8), created: time.Now().Unix(),
		finishAs:        "stop",
		filter:          stopFilter{sequences: req.Stop},
		tools:           map[string]*responseToolState{},
		byCall:          map[string]*responseToolState{},
		byIndex:         map[int]*responseToolState{},
		thoughts:        map[string]*responseReasoningState{},
		activeReasoning: "anonymous",
		searchDone:      map[string]bool{},
	}
	s.respWriter = w
	streamResponseHeaders(w)
	w.WriteHeader(http.StatusOK)
	s.stream = &httpserver.CheckedStreamWriter{Target: deadlineResponseWriter{ResponseWriter: w}}
	s.output = s.stream
	s.flusher = s.stream
	return s
}

// --- emitters ---------------------------------------------------------------

func (s *buildChatStream) emit(delta map[string]any, done string, usage map[string]any) error {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": nil}
	if done != "" {
		choice["finish_reason"] = done
	}
	chunk := map[string]any{"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.req.Model, "choices": []map[string]any{choice}}
	if usage != nil {
		chunk["usage"] = usage
	}
	if s.outcome.FirstToken.IsZero() && (delta["content"] != nil || delta["refusal"] != nil || delta["reasoning_content"] != nil || delta["tool_calls"] != nil) {
		s.outcome.FirstToken = time.Now()
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if err = s.stream.WriteSSE("", raw); err != nil {
		return err
	}
	s.flusher.Flush()
	return s.stream.Err
}

func (s *buildChatStream) fail(err error) {
	s.outcome.Err = err
	s.outcome.Finish = "error"
	writeSSEStreamError(s.respWriter, s.flusher, nil, err.Error())
}

func (s *buildChatStream) emitText(value string) error {
	if value == "" {
		return nil
	}
	s.text.WriteString(value)
	s.outcome.SawText = true
	return s.emit(map[string]any{"content": value}, "", nil)
}

// emitArgs streams tool arguments. A snapshot must extend what was streamed.
func (s *buildChatStream) emitArgs(tc *responseToolState, value string, snapshot bool) error {
	if snapshot {
		if value == "" {
			value = "{}"
		}
		current := tc.arguments.String()
		if !strings.HasPrefix(value, current) {
			return fmt.Errorf("tool %s arguments snapshot conflicts with streamed arguments", tc.callID)
		}
		value = strings.TrimPrefix(value, current)
	}
	if value == "" {
		return nil
	}
	tc.arguments.WriteString(value)
	return s.emit(map[string]any{"tool_calls": []map[string]any{{"index": tc.index, "function": map[string]any{"arguments": value}}}}, "", nil)
}

// toolItem registers a function_call item (once) and streams its arguments.
func (s *buildChatStream) toolItem(item, ev map[string]any) error {
	itemID, callID, name := strings.TrimSpace(responses.StreamString(item["id"])), strings.TrimSpace(responses.StreamString(item["call_id"])), strings.TrimSpace(responses.StreamString(item["name"]))
	if callID == "" {
		callID = itemID
	}
	if itemID == "" {
		itemID = callID
	}
	if name == "" || name == "<nil>" || callID == "" || callID == "<nil>" {
		return fmt.Errorf("upstream function_call missing valid name or call_id")
	}
	tc := s.tools[itemID]
	if tc == nil {
		if s.byCall[callID] != nil {
			return fmt.Errorf("duplicate upstream call_id %q", callID)
		}
		tc = &responseToolState{itemID: itemID, callID: callID, name: name, index: len(s.ordered)}
		s.tools[itemID], s.byCall[callID] = tc, tc
		s.ordered = append(s.ordered, tc)
		if index, exists := ev["output_index"]; exists {
			if prior := s.byIndex[responses.InterfaceToInt(index)]; prior != nil && prior != tc {
				return fmt.Errorf("upstream output_index reused by different tools")
			}
			s.byIndex[responses.InterfaceToInt(index)] = tc
		}
		if err := s.emit(map[string]any{"tool_calls": []map[string]any{{"index": tc.index, "id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": ""}}}}, "", nil); err != nil {
			return err
		}
	} else if tc.callID != callID || tc.name != name {
		return fmt.Errorf("upstream tool identity changed for %s", itemID)
	}
	if args, ok := item["arguments"]; ok && args != nil {
		value, ok := args.(string)
		if !ok {
			return fmt.Errorf("tool arguments must be a JSON string")
		}
		if value != "" {
			return s.emitArgs(tc, value, true)
		}
	}
	return nil
}

func (s *buildChatStream) thought(key string) *responseReasoningState {
	if key == "" {
		key = s.activeReasoning
	}
	if s.thoughts[key] == nil {
		s.thoughts[key] = &responseReasoningState{key: key}
	}
	return s.thoughts[key]
}

// emitThought streams reasoning text. One reasoning item keeps the first
// source it was seen from (raw text, summary or final snapshot).
func (s *buildChatStream) emitThought(key, source, value string) error {
	state := s.thought(key)
	if state.source == "" {
		state.source = source
	}
	if state.source != source || value == "" {
		return nil
	}
	state.text.WriteString(value)
	s.reasoning.WriteString(value)
	return s.emit(map[string]any{"reasoning_content": value, "reasoning_item_id": state.key}, "", nil)
}

// emitSignature sends a reasoning item's encrypted content when it changed.
func (s *buildChatStream) emitSignature(state *responseReasoningState, signature string) error {
	if signature == "" || signature == state.signature {
		return nil
	}
	state.signature = signature
	return s.emit(map[string]any{"reasoning_item_id": state.key, "reasoning_encrypted_content": signature}, "", nil)
}

// --- events -------------------------------------------------------------------

// onEvent handles one upstream Responses SSE event.
func (s *buildChatStream) onEvent(event, data string) error {
	if data == "[DONE]" {
		if !s.terminal {
			return fmt.Errorf("upstream stream ended without a response terminal event")
		}
		return io.EOF
	}
	var ev map[string]any
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return fmt.Errorf("build stream parse error: %w", err)
	}
	kind := util.FirstNonEmpty(chatwire.ParseLooseStringAny(ev["type"]), event)
	if kind == "error" || kind == "response.failed" {
		return responses.Failure(ev)
	}
	if s.terminal {
		return nil
	}
	s.annotations = appendUniqueResponsesAnnotations(s.annotations, responsesFlatAnnotations(ev))
	if model := responsesServedModel(ev); model != "" {
		s.outcome.ServedModel = model
	}
	if usage := responsesUsageFromStreamEvent(ev); len(usage) > 0 {
		s.outcome.Usage = usage
		s.outcome.UsageSource = audit.UsageSourceUpstream
	}
	item, _ := ev["item"].(map[string]any)
	if kind == "response.output_item.done" && item != nil {
		// Accumulate the portable items so the next turn can replay the whole
		// completed turn, not just its opaque reasoning cipher.
		switch chatwire.ParseLooseStringAny(item["type"]) {
		case "reasoning", "message", "function_call", "custom_tool_call":
			s.replayItems = append(s.replayItems, item)
		}
	}
	stopped := s.filter.matched != ""
	switch {
	case kind == "response.output_item.added" || kind == "response.output_item.done":
		return s.onOutputItem(kind, item, ev)
	case strings.HasPrefix(kind, "response.function_call_arguments."):
		if stopped {
			return nil
		}
		return s.onFunctionArguments(kind, ev)
	}
	if value := responsesReasoningDelta(kind, ev); value != "" && !stopped {
		source := "raw"
		if strings.Contains(kind, "summary") {
			source = "summary"
		}
		return s.emitThought(chatwire.ParseLooseStringAny(ev["item_id"]), source, value)
	}
	switch {
	case kind == "response.output_text.delta" && !stopped:
		s.sawText = true
		value, _ := ev["delta"].(string)
		return s.emitText(s.filter.push(value, false))
	case kind == "response.refusal.delta" && !stopped:
		value := responses.StreamString(ev["delta"])
		s.refusal.WriteString(value)
		return s.emit(map[string]any{"refusal": value}, "", nil)
	case kind == "response.completed" || kind == "response.incomplete":
		return s.onTerminal(kind, ev)
	}
	return nil
}

// onOutputItem handles output_item.added / .done for tools, searches and
// reasoning items.
func (s *buildChatStream) onOutputItem(kind string, item, ev map[string]any) error {
	stopped := s.filter.matched != ""
	done := kind == "response.output_item.done"
	switch chatwire.ParseLooseStringAny(item["type"]) {
	case "function_call":
		if !stopped {
			return s.toolItem(item, ev)
		}
	case "web_search_call", "x_search_call":
		if stopped {
			return nil
		}
		s.searchDone[searchIdentity(item)] = done
		return s.emit(map[string]any{"x_grok_search": item, "x_grok_search_done": done}, "", nil)
	case "reasoning":
		key := chatwire.ParseLooseStringAny(item["id"])
		if stopped {
			return nil
		}
		if key != "" && done && s.thoughts[key] == nil && s.activeReasoning == "anonymous" && s.thoughts["anonymous"] != nil {
			s.thoughts[key] = s.thoughts["anonymous"]
		}
		if key != "" {
			s.activeReasoning = key
		}
		state := s.thought(key)
		if done && state.text.Len() == 0 {
			value := responsesExtractReasoningText(map[string]any{"output": []any{item}})
			if err := s.emitThought(key, "snapshot", value); err != nil {
				return err
			}
		}
		if signature := responses.StreamString(item["encrypted_content"]); signature != "" && signature != state.signature {
			s.lastSignature = signature
			if err := s.emitSignature(state, signature); err != nil {
				return err
			}
		}
		if done {
			return s.emit(map[string]any{"reasoning_item_id": state.key, "reasoning_done": true}, "", nil)
		}
	}
	return nil
}

// onFunctionArguments streams argument deltas and checks the final snapshot.
func (s *buildChatStream) onFunctionArguments(kind string, ev map[string]any) error {
	itemID := chatwire.ParseLooseStringAny(ev["item_id"])
	tc := s.tools[itemID]
	if tc == nil && itemID == "" {
		if index, exists := ev["output_index"]; exists {
			tc = s.byIndex[responses.InterfaceToInt(index)]
		}
	}
	if tc == nil {
		return fmt.Errorf("arguments event references an unknown tool item")
	}
	if strings.HasSuffix(kind, ".delta") {
		value, _ := ev["delta"].(string)
		return s.emitArgs(tc, value, false)
	}
	if strings.HasSuffix(kind, ".done") {
		value, _ := ev["arguments"].(string)
		return s.emitArgs(tc, value, true)
	}
	return nil
}

// onTerminal takes the final response and fills in anything only the final
// output carries.
func (s *buildChatStream) onTerminal(kind string, ev map[string]any) error {
	response, _ := ev["response"].(map[string]any)
	terminalFinish, err := responses.TerminalFinish(kind, response)
	if err != nil {
		return err
	}
	s.finishAs = terminalFinish
	stopped := s.filter.matched != ""
	for _, raw := range responses.InterfaceSlice(response["output"]) {
		entry, _ := raw.(map[string]any)
		if stopped {
			continue
		}
		if err := s.onFinalOutput(entry); err != nil {
			return err
		}
	}
	// Some compatible servers only provide a final output snapshot.
	if s.refusal.Len() == 0 && !stopped {
		if value := responses.ExtractRefusal(response); value != "" {
			s.refusal.WriteString(value)
			if err := s.emit(map[string]any{"refusal": value}, "", nil); err != nil {
				return err
			}
		}
	}
	if !s.sawText && !stopped {
		if err := s.emitText(s.filter.push(responsesExtractMessageText(response), false)); err != nil {
			return err
		}
	}
	s.terminal = true
	return io.EOF
}

// onFinalOutput reconciles one item of the final output with what streamed.
func (s *buildChatStream) onFinalOutput(entry map[string]any) error {
	switch chatwire.ParseLooseStringAny(entry["type"]) {
	case "reasoning":
		key := chatwire.ParseLooseStringAny(entry["id"])
		state := s.thought(key)
		signature := chatwire.ParseLooseStringAny(entry["encrypted_content"])
		if signature != "" {
			s.lastSignature = signature
		}
		if state.text.Len() == 0 {
			if err := s.emitThought(key, "snapshot", responsesExtractReasoningText(map[string]any{"output": []any{entry}})); err != nil {
				return err
			}
		}
		if err := s.emitSignature(state, signature); err != nil {
			return err
		}
		return s.emit(map[string]any{"reasoning_item_id": state.key, "reasoning_done": true}, "", nil)
	case "web_search_call", "x_search_call":
		identity := searchIdentity(entry)
		if !s.searchDone[identity] {
			s.searchDone[identity] = true
			return s.emit(map[string]any{"x_grok_search": entry, "x_grok_search_done": true}, "", nil)
		}
	case "function_call":
		return s.toolItem(entry, nil)
	}
	return nil
}

// --- end of stream ------------------------------------------------------------

// finish validates the tool calls, sends the final chunk with usage and [DONE],
// and stores the reasoning replay for the next turn.
func (s *buildChatStream) finish() {
	if err := s.emitText(s.filter.push("", true)); err != nil {
		s.fail(err)
		return
	}
	var calls []map[string]any
	for _, tc := range s.ordered {
		if tc.arguments.Len() == 0 {
			if err := s.emitArgs(tc, "{}", false); err != nil {
				s.fail(err)
				return
			}
		}
		arguments := tc.arguments.String()
		if !json.Valid([]byte(arguments)) && s.finishAs != "length" {
			s.fail(fmt.Errorf("invalid JSON arguments for tool %s", tc.callID))
			return
		}
		calls = append(calls, map[string]any{"id": tc.callID, "type": "function", "function": map[string]any{"name": tc.name, "arguments": arguments}})
	}
	finish := s.finishAs
	if finish != "length" && len(calls) > 0 {
		finish = "tool_calls"
	}
	if s.filter.matched != "" {
		finish = "stop"
	}
	if s.text.Len() == 0 && s.refusal.Len() == 0 && len(calls) == 0 && finish != "length" && s.filter.matched == "" {
		s.fail(fmt.Errorf("upstream completed response with no content or tool calls"))
		return
	}
	outcome := s.outcome
	if outcome.Usage == nil {
		outcome.Usage = grokbase.AddReasoningUsage(buildChatUsagePayload(s.req, s.text.String()+s.refusal.String(), calls), s.reasoning.String())
		outcome.UsageSource = audit.UsageSourceEstimated
	}
	delta := map[string]any{}
	if len(s.annotations) > 0 {
		delta["annotations"] = responsesChatAnnotations(s.annotations)
	}
	if s.filter.matched != "" {
		delta["stop_sequence"] = s.filter.matched
	}
	if err := s.emit(delta, finish, outcome.Usage); err != nil {
		outcome.Err = err
		return
	}
	if _, err := io.WriteString(s.output, "data: [DONE]\n\n"); err != nil {
		outcome.Err = err
		return
	}
	s.flusher.Flush()
	if s.stream.Err != nil {
		outcome.Err = s.stream.Err
		return
	}
	outcome.Finish = finish
	if s.req.ReasoningReplay {
		if len(s.replayItems) > 0 {
			s.h.captureReasoningReplayItems(context.Background(), s.req.Model, s.req.PromptCacheKey, s.replayItems)
		} else if s.lastSignature != "" {
			s.h.storeReasoningReplay(s.req.Model, s.req.PromptCacheKey, s.lastSignature)
		}
	}
}
