package responses

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"encoding/json"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/util"
)

// One state per output item; late signatures and snapshots update that item,
// never a single global reasoning buffer. Public IDs remain stable throughout.
type chatResponseItem struct {
	index     int
	value     map[string]interface{}
	text      strings.Builder
	signature string
	closed    bool
}

// chatStreamOptions configures one chat-to-Responses translation.
type StreamOptions struct {
	// onComplete receives the terminal response object after it is built.
	OnComplete func(map[string]interface{})
	// SearchHook turns a provider-private search delta into a Responses output
	// item. Nil means the channel has no private search traffic, so the
	// translator stays free of provider knowledge.
	SearchHook SearchItemHook
}

// SearchItemHook reports the output item a chat delta carries for a provider's
// private search channel. It returns a nil value when the delta has none.
//
// The hook exists because a search item is the one part of a chat stream that
// has no Responses equivalent: it is injected by this gateway's own
// provider-to-chat layer, under a private key the protocol never defines.
type SearchItemHook func(delta map[string]interface{}) (identity string, value map[string]interface{}, done bool)

// WriteStreamFromChatReader is the translation itself.
func WriteStreamFromChatReader(w http.ResponseWriter, request CreateRequest, reader io.Reader, opts StreamOptions) {
	httpserver.StreamResponseHeaders(w)
	writer := &httpserver.CheckedStreamWriter{Target: w}
	id := "resp_" + util.RandomHex(12)
	created := time.Now().Unix()
	items := []*chatResponseItem{}
	thoughts := map[string]*chatResponseItem{}
	tools := map[int]*chatResponseItem{}
	callIDs := map[string]bool{}
	searches := map[string]*chatResponseItem{}
	var message *chatResponseItem
	textIndex, refusalIndex := -1, -1
	var text, refusal strings.Builder
	annotations := []interface{}{}
	annotationKeys := map[string]bool{}
	activeThought := ""
	var usage map[string]interface{}
	finish := ""
	sawDone := false
	meaningful := false
	sequence := 0
	emit := func(kind string, payload map[string]interface{}) {
		// sequence_number must increase by one per event: a client that
		// reconnects with Last-Event-ID asks for everything after the last
		// number it saw, and an event without one cannot be ordered at all.
		payload["type"] = kind
		payload["sequence_number"] = sequence
		sequence++
		data, err := json.Marshal(payload)
		if err != nil {
			writer.Err = err
			return
		}
		_, _ = fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", kind, data)
		writer.Flush()
	}
	response := func(status string) map[string]interface{} {
		out := make([]interface{}, 0, len(items))
		for _, item := range items {
			out = append(out, item.value)
		}
		v := map[string]interface{}{"id": id, "object": "response", "created_at": created, "model": request.Model, "status": status, "output": out}
		if usage != nil {
			v["usage"] = usage
		}
		if len(request.Metadata) > 0 {
			v["metadata"] = request.Metadata
		}
		if request.Truncation != "" {
			v["truncation"] = request.Truncation
		}
		return v
	}
	add := func(value map[string]interface{}) *chatResponseItem {
		item := &chatResponseItem{index: len(items), value: value}
		items = append(items, item)
		emit("response.output_item.added", map[string]interface{}{"output_index": item.index, "item": value})
		return item
	}
	ensureMessage := func() {
		if message == nil {
			message = add(map[string]interface{}{"id": "msg_" + util.RandomHex(12), "type": "message", "role": "assistant", "status": "in_progress", "content": []interface{}{}})
		}
	}
	emit("response.created", map[string]interface{}{"response": response("in_progress")})
	if writer.Err != nil {
		return
	}
	err := ReadSSEBytes(reader, func(event string, data []byte) error {
		if writer.Err != nil {
			return writer.Err
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			sawDone = true
			return io.EOF
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal(data, &chunk); err != nil {
			return fmt.Errorf("invalid chat SSE: %w", err)
		}
		if chunk == nil {
			return fmt.Errorf("chat SSE must be an object")
		}
		if chunk["error"] != nil || event == "error" {
			return Failure(chunk)
		}
		if u, ok := chunk["usage"].(map[string]interface{}); ok {
			usage = UsageFromChat(u)
		}
		for _, raw := range InterfaceSlice(chunk["choices"]) {
			choice, ok := raw.(map[string]interface{})
			if !ok {
				return fmt.Errorf("invalid chat choice")
			}
			if index, exists := choice["index"]; exists && InterfaceToInt(index) != 0 {
				return fmt.Errorf("multiple chat choices are not supported by Responses")
			}
			delta, _ := choice["delta"].(map[string]interface{})
			if finish != "" && len(delta) > 0 {
				return fmt.Errorf("chat output after finish_reason")
			}
			thinking := StreamString(FirstNonNil(delta["reasoning_content"], delta["reasoning"]))
			signature := StreamString(delta["reasoning_encrypted_content"])
			key := StreamString(delta["reasoning_item_id"])
			if thinking != "" || signature != "" {
				if key == "" {
					key = activeThought
					if key == "" {
						key = "anonymous_" + util.RandomHex(8)
					}
				}
				activeThought = key
				state := thoughts[key]
				if state == nil {
					state = add(map[string]interface{}{"id": "rs_" + util.RandomHex(12), "type": "reasoning", "status": "in_progress", "summary": []interface{}{}})
					thoughts[key] = state
					emit("response.reasoning_summary_part.added", map[string]interface{}{"item_id": state.value["id"], "output_index": state.index, "summary_index": 0, "part": map[string]interface{}{"type": "summary_text", "text": ""}})
				}
				if thinking != "" {
					if state.closed {
						return fmt.Errorf("reasoning delta after item completion")
					}
					state.text.WriteString(thinking)
					emit("response.reasoning_summary_text.delta", map[string]interface{}{"item_id": state.value["id"], "output_index": state.index, "summary_index": 0, "delta": thinking})
				}
				if signature != "" {
					state.signature = signature
				}
				meaningful = true
			}
			if done, _ := delta["reasoning_done"].(bool); done {
				if key == "" {
					key = activeThought
				}
				if state := thoughts[key]; state != nil {
					state.closed = true
				}
				activeThought = ""
			}
			// A provider-private search delta becomes a Responses output item
			// only when the caller registered a hook. x_grok_search is Grok
			// Build's private spelling; the hook keeps that knowledge out of
			// this package.
			if opts.SearchHook != nil {
				key, value, done := opts.SearchHook(delta)
				if value != nil {
					state := searches[key]
					if state == nil {
						state = add(value)
						searches[key] = state
					}
					for k, v := range value {
						if k != "id" && k != "status" {
							state.value[k] = v
						}
					}
					if done {
						state.closed = true
						state.value["status"] = value["status"]
					}
					meaningful = true
					activeThought = ""
				}
			}
			for _, kind := range []string{"content", "refusal"} {
				value := StreamString(delta[kind])
				if value == "" {
					continue
				}
				meaningful = true
				activeThought = ""
				ensureMessage()
				partIndex := &textIndex
				partType := "output_text"
				builder := &text
				if kind == "refusal" {
					partIndex = &refusalIndex
					partType = "refusal"
					builder = &refusal
				}
				if *partIndex < 0 {
					parts := InterfaceSlice(message.value["content"])
					*partIndex = len(parts)
					part := map[string]interface{}{"type": partType}
					if kind == "content" {
						part["text"] = ""
						part["annotations"] = []interface{}{}
					} else {
						part["refusal"] = ""
					}
					message.value["content"] = append(parts, part)
					emit("response.content_part.added", map[string]interface{}{"item_id": message.value["id"], "output_index": message.index, "content_index": *partIndex, "part": part})
				}
				builder.WriteString(value)
				emit("response."+partType+".delta", map[string]interface{}{"item_id": message.value["id"], "output_index": message.index, "content_index": *partIndex, "delta": value})
			}
			for _, ann := range Annotations(delta["annotations"]) {
				b, _ := json.Marshal(ann)
				key := string(b)
				if !annotationKeys[key] {
					annotationKeys[key] = true
					annotations = append(annotations, ann)
				}
			}
			for _, raw := range InterfaceSlice(delta["tool_calls"]) {
				call, ok := raw.(map[string]interface{})
				if !ok {
					return fmt.Errorf("invalid tool call")
				}
				fn, _ := call["function"].(map[string]interface{})
				index := InterfaceToInt(call["index"])
				if index < 0 {
					return fmt.Errorf("invalid tool index")
				}
				callID, name := StreamString(call["id"]), StreamString(fn["name"])
				state := tools[index]
				if state == nil {
					if strings.TrimSpace(callID) == "" || callID == "<nil>" || strings.TrimSpace(name) == "" || name == "<nil>" || callIDs[callID] {
						return fmt.Errorf("invalid or duplicate tool identity")
					}
					callIDs[callID] = true
					// A grouped tool travels upstream as namespace__name; the
					// item is put back on the name the caller declared straight
					// away, so every event already carries it.
					item := map[string]interface{}{"id": "fc_" + util.RandomHex(12), "type": "function_call", "call_id": callID, "name": name, "arguments": "", "status": "in_progress"}
					state = add(item)
					tools[index] = state
				} else if (callID != "" && callID != state.value["call_id"]) || (name != "" && name != state.value["name"]) {
					return fmt.Errorf("tool identity changed")
				}
				if fragment, exists := fn["arguments"]; exists {
					value, ok := fragment.(string)
					if !ok {
						return fmt.Errorf("tool arguments must be a string")
					}
					state.text.WriteString(value)
					if value != "" {
						emit("response.function_call_arguments.delta", map[string]interface{}{"item_id": state.value["id"], "output_index": state.index, "delta": value})
					}
				}
				meaningful = true
				activeThought = ""
			}
			if end := StreamString(choice["finish_reason"]); end != "" {
				switch end {
				case "stop", "tool_calls", "length", "content_filter":
					finish = end
				default:
					return fmt.Errorf("unsupported finish_reason %q", end)
				}
			}
		}
		return writer.Err
	})
	if err == io.EOF {
		err = nil
	}
	if err == nil && (!sawDone || finish == "") {
		err = fmt.Errorf("chat stream ended without finish_reason and [DONE]")
	}
	if err == nil && !meaningful && finish != "length" && finish != "content_filter" {
		err = fmt.Errorf("chat stream completed without output")
	}
	status, details := StatusFromFinish(finish)
	for _, item := range items {
		if item.value["type"] == "function_call" {
			args := item.text.String()
			if args == "" {
				args = "{}"
			}
			if status == "completed" && !json.Valid([]byte(args)) && err == nil {
				err = fmt.Errorf("invalid completed tool arguments")
			}
			item.value["arguments"] = args
		}
	}
	if writer.Err != nil {
		return
	}
	if err != nil {
		v := response("failed")
		v["error"] = map[string]interface{}{"code": "upstream_stream_error", "message": err.Error()}
		emit("response.failed", map[string]interface{}{"response": v})
		_, _ = io.WriteString(writer, "data: [DONE]\n\n")
		writer.Flush()
		return
	}
	for _, item := range items {
		kind := StreamString(item.value["type"])
		itemStatus := status
		if kind == "web_search_call" && item.closed {
			itemStatus = ParseLooseStringAny(item.value["status"])
		}
		item.value["status"] = itemStatus
		switch kind {
		case "reasoning":
			part := map[string]interface{}{"type": "summary_text", "text": item.text.String()}
			item.value["summary"] = []interface{}{part}
			if item.signature != "" {
				item.value["encrypted_content"] = item.signature
			}
			emit("response.reasoning_summary_text.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "summary_index": 0, "text": item.text.String()})
			emit("response.reasoning_summary_part.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "summary_index": 0, "part": part})
		case "function_call":
			emit("response.function_call_arguments.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "arguments": item.value["arguments"]})
		case "message":
			parts := InterfaceSlice(item.value["content"])
			for i, raw := range parts {
				part := raw.(map[string]interface{})
				partType := StreamString(part["type"])
				fields := map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "content_index": i}
				if partType == "output_text" {
					part["text"] = text.String()
					part["annotations"] = annotations
					fields["text"] = text.String()
					for n, ann := range annotations {
						emit("response.output_text.annotation.added", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "content_index": i, "annotation_index": n, "annotation": ann})
					}
				} else {
					part["refusal"] = refusal.String()
					fields["refusal"] = refusal.String()
				}
				emit("response."+partType+".done", fields)
				emit("response.content_part.done", map[string]interface{}{"item_id": item.value["id"], "output_index": item.index, "content_index": i, "part": part})
			}
		}
		emit("response.output_item.done", map[string]interface{}{"output_index": item.index, "item": item.value})
	}
	if usage == nil {
		usage = UsageFromChat(nil)
	}
	v := response(status)
	if details != nil {
		v["incomplete_details"] = details
	}
	if opts.OnComplete != nil {
		opts.OnComplete(v)
	}
	emit("response."+status, map[string]interface{}{"response": v})
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	writer.Flush()
}
