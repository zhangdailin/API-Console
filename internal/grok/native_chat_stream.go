package grok

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/responses"
	"orchids-api/internal/util"
	"strings"
	"time"
	"unicode/utf8"

	"encoding/json"

	"orchids-api/internal/audit"
)

type chatOutcome struct {
	// Withheld marks a turn the quality hold refused to deliver. Nothing was
	// written to the client, so the caller may retry it on another account.
	Withheld    bool
	Usage       map[string]interface{}
	UsageSource audit.UsageSource
	Finish      string
	FirstToken  time.Time
	Err         error
	// Quality carries what the quality guard needs to tell a healthy turn from
	// a degraded one (see quality_guard.go).
	Quality qualitySignals
	SawText bool
}

// stopFilter keeps only a suffix that could still become a stop sequence.
// It operates on the original deltas: trimming whitespace corrupts tool JSON
// and text, and a stop sequence can span multiple upstream events.
type stopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func (f *stopFilter) push(delta string, flush bool) string {
	if f.matched != "" {
		return ""
	}
	f.pending += delta
	cut, match := len(f.pending), ""
	for _, stop := range f.sequences {
		if stop == "" {
			continue
		}
		if i := strings.Index(f.pending, stop); i >= 0 && (match == "" || i < cut) {
			cut, match = i, stop
		}
	}
	if match != "" {
		out := f.pending[:cut]
		f.pending = ""
		f.matched = match
		return out
	}
	keep := 0
	if !flush {
		for _, stop := range f.sequences {
			for n := 1; n < len(stop) && n <= len(f.pending); n++ {
				if strings.HasSuffix(f.pending, stop[:n]) && n > keep {
					keep = n
				}
			}
		}
	}
	cut = len(f.pending) - keep
	for cut > 0 && !utf8.ValidString(f.pending[:cut]) {
		cut--
	}
	out := f.pending[:cut]
	f.pending = f.pending[cut:]
	return out
}

type responseToolState struct {
	itemID, callID, name string
	arguments            strings.Builder
	index                int
}
type responseReasoningState struct {
	source, signature, key string
	text                   strings.Builder
}

// streamBuildChatHolding is the Build streaming entry with the quality hold
// attached.
//
// While a hold is active nothing is written to the client: the deferred writer
// buffers frames, and the classifier decides after every content event whether
// to release them (deliver), keep waiting, or drop them and let the caller retry
// on another account (withhold). A nil hold keeps the plain streaming path.
func (h *Handler) streamBuildChatHolding(w http.ResponseWriter, req *chatwire.Request, body io.Reader, hold *buildQualityHold) (outcome chatOutcome) {
	outcomeStarted := time.Now()
	if hold != nil {
		// The hold reads this stream's own accounting, so point it at the outcome
		// being built. It never writes to it.
		hold.outcome = &outcome
		hold.started = outcomeStarted
	}
	defer func() {
		outcome.Quality.ExpectReasoning = qualityExpectsReasoning(req, false)
		outcome.Quality.Terminal = outcome.Err == nil
		if outcome.Usage != nil {
			if details, _ := outcome.Usage["completion_tokens_details"].(map[string]interface{}); details != nil {
				outcome.Quality.ReasoningTokens = int64(responses.InterfaceToInt(details["reasoning_tokens"]))
			}
		}
	}()
	var output io.Writer = w
	var respWriter http.ResponseWriter = w
	var flusher http.Flusher
	if hold != nil {
		output, respWriter, flusher = hold.writer, hold.writer, hold.writer
	} else {
		flusher = streamResponseHeaders(w)
	}
	// withheld aborts the read loop as soon as the classifier refuses the turn.
	var withheld bool
	checkHold := func(terminalEvent bool) error {
		if hold == nil {
			return nil
		}
		verdict, waiting := hold.step(terminalEvent)
		if waiting {
			return nil
		}
		if verdict == qualityWithhold {
			withheld = true
			return errQualityWithheld
		}
		return hold.writer.Commit()
	}
	id, created := "chatcmpl_"+util.RandomHex(8), time.Now().Unix()
	var text, reasoning, refusal strings.Builder
	tools := map[string]*responseToolState{}
	byCall := map[string]*responseToolState{}
	byIndex := map[int]*responseToolState{}
	var ordered []*responseToolState
	thoughts := map[string]*responseReasoningState{}
	var annotations []map[string]interface{}
	filter := stopFilter{sequences: req.Stop}
	terminal := false
	sawText := false
	lastSignature := ""
	var replayItems []interface{}
	activeReasoning := "anonymous"
	searchDone := map[string]bool{}
	finish := "stop"
	hideReasoning := h.hideReasoning()
	emit := func(delta map[string]interface{}, done string, usage map[string]interface{}) error {
		if hideReasoning && hideChatReasoning(delta) && len(delta) == 0 && done == "" && usage == nil {
			return checkHold(false)
		}
		choice := map[string]interface{}{"index": 0, "delta": delta, "finish_reason": nil}
		if done != "" {
			choice["finish_reason"] = done
		}
		chunk := map[string]interface{}{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": []map[string]interface{}{choice}}
		if usage != nil {
			chunk["usage"] = usage
		}
		if outcome.FirstToken.IsZero() && (delta["content"] != nil || delta["refusal"] != nil || delta["reasoning_content"] != nil || delta["tool_calls"] != nil) {
			outcome.FirstToken = time.Now()
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if _, err = fmt.Fprintf(output, "data: %s\n\n", raw); err != nil {
			return err
		}
		flusher.Flush()
		return checkHold(false)
	}
	fail := func(err error) {
		outcome.Err = err
		outcome.Finish = "error"
		if hold != nil {
			// An upstream error is not a quality dump: it belongs to
			// retryWithAccountSwitch, which retries before any body exists. Release
			// whatever was held so the caller sees a real error instead of silence.
			if commitErr := hold.writer.Commit(); commitErr != nil {
				return
			}
		}
		writeSSEStreamError(respWriter, flusher, nil, err.Error())
	}
	if err := emit(map[string]interface{}{"role": "assistant"}, "", nil); err != nil {
		outcome.Err = err
		return
	}
	emitText := func(value string) error {
		if value == "" {
			return nil
		}
		text.WriteString(value)
		outcome.Quality.VisibleChars += int64(len(value))
		outcome.SawText = true
		if outcome.Quality.FirstVisibleMS < 0 {
			outcome.Quality.FirstVisibleMS = time.Since(outcomeStarted).Milliseconds()
		}
		return emit(map[string]interface{}{"content": value}, "", nil)
	}
	emitArgs := func(tc *responseToolState, value string, snapshot bool) error {
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
		return emit(map[string]interface{}{"tool_calls": []map[string]interface{}{{"index": tc.index, "function": map[string]interface{}{"arguments": value}}}}, "", nil)
	}
	toolItem := func(item, ev map[string]interface{}) error {
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
		tc := tools[itemID]
		if tc == nil {
			outcome.Quality.ToolCalls++
			if byCall[callID] != nil {
				return fmt.Errorf("duplicate upstream call_id %q", callID)
			}
			tc = &responseToolState{itemID: itemID, callID: callID, name: name, index: len(ordered)}
			tools[itemID], byCall[callID] = tc, tc
			ordered = append(ordered, tc)
			if index, exists := ev["output_index"]; exists {
				if prior := byIndex[responses.InterfaceToInt(index)]; prior != nil && prior != tc {
					return fmt.Errorf("upstream output_index reused by different tools")
				}
				byIndex[responses.InterfaceToInt(index)] = tc
			}
			if err := emit(map[string]interface{}{"tool_calls": []map[string]interface{}{{"index": tc.index, "id": callID, "type": "function", "function": map[string]interface{}{"name": name, "arguments": ""}}}}, "", nil); err != nil {
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
				return emitArgs(tc, value, true)
			}
		}
		return nil
	}
	thought := func(key string) *responseReasoningState {
		if key == "" {
			key = activeReasoning
		}
		if thoughts[key] == nil {
			thoughts[key] = &responseReasoningState{key: key}
		}
		return thoughts[key]
	}
	emitThought := func(key, source, value string) error {
		if hold != nil {
			hold.markReasoningStarted()
		}
		state := thought(key)
		if state.source == "" {
			state.source = source
		}
		if state.source != source || value == "" {
			return nil
		}
		state.text.WriteString(value)
		reasoning.WriteString(value)
		outcome.Quality.SawReasoning = true
		outcome.Quality.ReasoningChars += int64(len(value))
		return emit(map[string]interface{}{"reasoning_content": value, "reasoning_item_id": state.key}, "", nil)
	}
	err := responses.ReadSSE(body, func(event, data string) error {
		if withheld {
			return errQualityWithheld
		}
		if data == "[DONE]" {
			if !terminal {
				return fmt.Errorf("upstream stream ended without a response terminal event")
			}
			return io.EOF
		}
		var ev map[string]interface{}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return fmt.Errorf("build stream parse error: %w", err)
		}
		kind := util.FirstNonEmpty(chatwire.ParseLooseStringAny(ev["type"]), event)
		if kind == "error" || kind == "response.failed" {
			return responses.Failure(ev)
		}
		if terminal {
			return nil
		}
		annotations = appendUniqueConsoleAnnotations(annotations, consoleFlatAnnotations(ev))
		if usage := consoleUsageFromStreamEvent(ev); len(usage) > 0 {
			outcome.Usage = usage
			outcome.UsageSource = audit.UsageSourceUpstream
		}
		item, _ := ev["item"].(map[string]interface{})
		if kind == "response.output_item.done" && item != nil {
			// Accumulate the portable items so the next turn can replay the whole
			// completed turn, not just its opaque reasoning cipher.
			switch chatwire.ParseLooseStringAny(item["type"]) {
			case "reasoning", "message", "function_call", "custom_tool_call":
				replayItems = append(replayItems, item)
			}
		}
		if kind == "response.output_item.added" || kind == "response.output_item.done" {
			switch chatwire.ParseLooseStringAny(item["type"]) {
			case "function_call":
				if filter.matched == "" {
					return toolItem(item, ev)
				}
			case "web_search_call", "x_search_call":
				if filter.matched != "" {
					return nil
				}
				searchDone[searchIdentity(item)] = kind == "response.output_item.done"
				return emit(map[string]interface{}{"x_grok_search": item, "x_grok_search_done": kind == "response.output_item.done"}, "", nil)
			case "reasoning":
				if hold != nil {
					hold.markReasoningStarted()
				}
				key := chatwire.ParseLooseStringAny(item["id"])
				if filter.matched != "" {
					return nil
				}
				if key != "" && kind == "response.output_item.done" && thoughts[key] == nil && activeReasoning == "anonymous" && thoughts["anonymous"] != nil {
					thoughts[key] = thoughts["anonymous"]
				}
				if key != "" {
					activeReasoning = key
				}
				state := thought(key)
				if kind == "response.output_item.done" {
					if state.text.Len() == 0 {
						value := consoleExtractReasoningText(map[string]interface{}{"output": []interface{}{item}})
						if err := emitThought(key, "snapshot", value); err != nil {
							return err
						}
					}
				}
				if signature := responses.StreamString(item["encrypted_content"]); signature != "" && signature != state.signature {
					outcome.Quality.EncryptedChars = int64(len(signature))
					state.signature = signature
					lastSignature = signature
					if err := emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_encrypted_content": signature}, "", nil); err != nil {
						return err
					}
				}
				if kind == "response.output_item.done" {
					return emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_done": true}, "", nil)
				}
			}
			return nil
		}
		if strings.HasPrefix(kind, "response.function_call_arguments.") {
			if filter.matched != "" {
				return nil
			}
			itemID := chatwire.ParseLooseStringAny(ev["item_id"])
			tc := tools[itemID]
			if tc == nil && itemID == "" {
				if index, exists := ev["output_index"]; exists {
					tc = byIndex[responses.InterfaceToInt(index)]
				}
			}
			if tc == nil {
				return fmt.Errorf("arguments event references an unknown tool item")
			}
			if strings.HasSuffix(kind, ".delta") {
				value, _ := ev["delta"].(string)
				return emitArgs(tc, value, false)
			}
			if strings.HasSuffix(kind, ".done") {
				value, _ := ev["arguments"].(string)
				return emitArgs(tc, value, true)
			}
			return nil
		}
		if value := consoleReasoningDelta(kind, ev); value != "" && filter.matched == "" {
			source := "raw"
			if strings.Contains(kind, "summary") {
				source = "summary"
			}
			return emitThought(chatwire.ParseLooseStringAny(ev["item_id"]), source, value)
		}
		if kind == "response.output_text.delta" && filter.matched == "" {
			sawText = true
			value, _ := ev["delta"].(string)
			return emitText(filter.push(value, false))
		}
		if kind == "response.refusal.delta" && filter.matched == "" {
			value := responses.StreamString(ev["delta"])
			refusal.WriteString(value)
			return emit(map[string]interface{}{"refusal": value}, "", nil)
		}
		if kind == "response.completed" || kind == "response.incomplete" {
			response, _ := ev["response"].(map[string]interface{})
			terminalFinish, err := responses.TerminalFinish(kind, response)
			if err != nil {
				return err
			}
			finish = terminalFinish
			for _, raw := range responses.InterfaceSlice(response["output"]) {
				entry, _ := raw.(map[string]interface{})
				if filter.matched != "" {
					continue
				}
				switch chatwire.ParseLooseStringAny(entry["type"]) {
				case "reasoning":
					key := chatwire.ParseLooseStringAny(entry["id"])
					state := thought(key)
					signature := chatwire.ParseLooseStringAny(entry["encrypted_content"])
					if signature != "" {
						lastSignature = signature
					}
					if state.text.Len() == 0 {
						if err := emitThought(key, "snapshot", consoleExtractReasoningText(map[string]interface{}{"output": []interface{}{entry}})); err != nil {
							return err
						}
					}
					if signature != "" && signature != state.signature {
						state.signature = signature
						if err := emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_encrypted_content": signature}, "", nil); err != nil {
							return err
						}
					}
					if err := emit(map[string]interface{}{"reasoning_item_id": state.key, "reasoning_done": true}, "", nil); err != nil {
						return err
					}
				case "web_search_call", "x_search_call":
					identity := searchIdentity(entry)
					if !searchDone[identity] {
						searchDone[identity] = true
						if err := emit(map[string]interface{}{"x_grok_search": entry, "x_grok_search_done": true}, "", nil); err != nil {
							return err
						}
					}
				case "function_call":
					if err := toolItem(entry, nil); err != nil {
						return err
					}
				}
			}
			// Some compatible servers only provide a final output snapshot.
			if refusal.Len() == 0 && filter.matched == "" {
				if value := responses.ExtractRefusal(response); value != "" {
					refusal.WriteString(value)
					if err := emit(map[string]interface{}{"refusal": value}, "", nil); err != nil {
						return err
					}
				}
			}
			if !sawText && filter.matched == "" {
				if err := emitText(filter.push(consoleExtractMessageText(response), false)); err != nil {
					return err
				}
			}
			terminal = true
			// The turn is over: the classifier can now judge what arrived and
			// release a healthy answer instead of holding it to the timeout.
			if holdErr := checkHold(true); holdErr != nil {
				return holdErr
			}
			return io.EOF
		}
		return nil
	})
	if errors.Is(err, errQualityWithheld) || withheld {
		outcome.Withheld = true
		outcome.Quality.Terminal = true
		outcome.Finish = "quality_degraded"
		return
	}
	if err != nil && err != io.EOF {
		fail(fmt.Errorf("build stream read error: %w", err))
		return
	}
	if !terminal {
		fail(fmt.Errorf("upstream stream closed before response completion"))
		return
	}
	if err := emitText(filter.push("", true)); err != nil {
		fail(err)
		return
	}
	var calls []map[string]interface{}
	for _, tc := range ordered {
		if tc.arguments.Len() == 0 {
			if err := emitArgs(tc, "{}", false); err != nil {
				fail(err)
				return
			}
		}
		arguments := tc.arguments.String()
		if !json.Valid([]byte(arguments)) && finish != "length" {
			fail(fmt.Errorf("invalid JSON arguments for tool %s", tc.callID))
			return
		}
		calls = append(calls, map[string]interface{}{"id": tc.callID, "type": "function", "function": map[string]interface{}{"name": tc.name, "arguments": arguments}})
	}
	if finish != "length" && len(calls) > 0 {
		finish = "tool_calls"
	}
	if filter.matched != "" {
		finish = "stop"
	}
	if text.Len() == 0 && refusal.Len() == 0 && len(calls) == 0 && finish != "length" && filter.matched == "" {
		fail(fmt.Errorf("upstream completed response with no content or tool calls"))
		return
	}
	if outcome.Usage == nil {
		outcome.Usage = addReasoningUsage(buildChatUsagePayload(req, text.String()+refusal.String(), calls), reasoning.String())
		outcome.UsageSource = audit.UsageSourceEstimated
	}
	delta := map[string]interface{}{}
	if len(annotations) > 0 {
		delta["annotations"] = consoleChatAnnotations(annotations)
	}
	if filter.matched != "" {
		delta["stop_sequence"] = filter.matched
	}
	if err := emit(delta, finish, outcome.Usage); err != nil {
		outcome.Err = err
		return
	}
	if _, err := io.WriteString(output, "data: [DONE]\n\n"); err != nil {
		outcome.Err = err
		return
	}
	flusher.Flush()
	outcome.Finish = finish
	if req.ReasoningReplay {
		if len(replayItems) > 0 {
			h.captureReasoningReplayItems(context.Background(), req.Model, req.PromptCacheKey, replayItems)
		} else if lastSignature != "" {
			h.storeReasoningReplay(req.Model, req.PromptCacheKey, lastSignature)
		}
	}
	return
}
