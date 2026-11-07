package grok

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"math"
	"net/http"
	"strconv"
	"strings"

	"orchids-api/internal/responses"
	"orchids-api/internal/store"
	"orchids-api/internal/upstream"
)

// Function-call arguments are held only until their call completes, so text and
// reasoning keep streaming. Past these bounds a call's arguments pass through
// unrepaired instead of growing gateway memory.
const (
	maxHeldFunctionArgumentsBytes  = 1 << 20
	maxTotalHeldFunctionArgsBytes  = 4 << 20
	maxRepairableArgumentNumberLen = 64
)

// doResponsesWithToolRepair streams a Build response straight through and
// repairs only declared function-call arguments, the way grok2api does: text,
// reasoning and every other event are delivered as they arrive. Arguments that
// still violate the schema after repair are delivered as generated; the caller
// that executes the tool reports the problem back to the model, which is what
// the first-party APIs do too.
func (c *CLIClient) doResponsesWithToolRepair(ctx context.Context, acc *store.Account, path string, payload map[string]any) (*http.Response, error) {
	schemas := functionToolSchemas(path, payload)
	resp, err := c.doResponsesUncheckedAt(ctx, acc, path, payload)
	if err != nil || len(schemas) == 0 || resp.Body == nil {
		return resp, err
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		resp.Body = newToolArgumentsRepairStream(resp.Body, schemas)
		return resp, nil
	}
	raw, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(repairResponseJSON(raw, schemas)))
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

func functionToolSchemas(path string, payload map[string]any) map[string]map[string]any {
	if strings.Trim(path, "/") != "responses" {
		return nil
	}
	schemas := map[string]map[string]any{}
	for _, tool := range responses.InterfaceMaps(payload["tools"]) {
		if tool["type"] != "function" {
			continue
		}
		if schema, ok := tool["parameters"].(map[string]any); ok && len(schema) > 0 {
			schemas[responses.StreamString(tool["name"])] = schema
		}
	}
	return schemas
}

// repairFunctionArguments returns arguments with properties the schema forbids
// removed and integral numbers spelled as integers where the schema wants an
// integer. Unparseable or already valid arguments are returned unchanged, byte
// for byte.
func repairFunctionArguments(name, arguments string, schema map[string]any, report bool) string {
	if strings.TrimSpace(arguments) == "" || schema == nil {
		return arguments
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return arguments
	}
	repaired, changed := repairArgumentValue(value, schema, schema, 0)
	if changed {
		if encoded, err := json.Marshal(repaired); err == nil {
			arguments = string(encoded)
			value = repaired
		}
	}
	if !report {
		return arguments
	}
	if err := upstream.CompileSchema(schema).Validate(jsonNumbersToFloat(value)); err != nil {
		slog.Warn("Grok function arguments do not match the declared schema; delivering as generated", "tool", name, "error", err)
	}
	return arguments
}

func repairArgumentValue(value any, schema, root map[string]any, depth int) (any, bool) {
	if depth > 64 || schema == nil {
		return value, false
	}
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
		defs, _ := root["$defs"].(map[string]any)
		if target, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]any); ok {
			return repairArgumentValue(value, target, root, depth+1)
		}
		return value, false
	}
	switch typed := value.(type) {
	case json.Number:
		if schemaWantsInteger(schema) {
			if normalized, ok := integralNumber(typed); ok {
				return normalized, true
			}
		}
	case map[string]any:
		properties, _ := schema["properties"].(map[string]any)
		changed := false
		// Only a plain object schema says exactly which keys exist. Combinators
		// can declare more, so nothing is removed under them.
		if closed, ok := schema["additionalProperties"].(bool); ok && !closed && properties != nil && !schemaHasCombinator(schema) {
			for key := range typed {
				if _, declared := properties[key]; !declared {
					delete(typed, key)
					changed = true
				}
			}
		}
		for key, item := range typed {
			property, _ := properties[key].(map[string]any)
			if repaired, current := repairArgumentValue(item, property, root, depth+1); current {
				typed[key], changed = repaired, true
			}
		}
		return typed, changed
	case []any:
		items, _ := schema["items"].(map[string]any)
		changed := false
		for index, item := range typed {
			if repaired, current := repairArgumentValue(item, items, root, depth+1); current {
				typed[index], changed = repaired, true
			}
		}
		return typed, changed
	}
	return value, false
}

func schemaHasCombinator(schema map[string]any) bool {
	for _, keyword := range []string{"allOf", "anyOf", "oneOf", "patternProperties"} {
		if _, ok := schema[keyword]; ok {
			return true
		}
	}
	return false
}

func schemaWantsInteger(schema map[string]any) bool {
	switch kind := schema["type"].(type) {
	case string:
		return kind == "integer"
	case []any:
		integer := false
		for _, item := range kind {
			if item == "number" {
				return false
			}
			integer = integer || item == "integer"
		}
		return integer
	}
	return false
}

// integralNumber rewrites 60000.0 or 6e4 as 60000. Values outside the exactly
// representable integer range are left alone rather than rounded.
func integralNumber(number json.Number) (json.Number, bool) {
	raw := number.String()
	if len(raw) > maxRepairableArgumentNumberLen || !strings.ContainsAny(raw, ".eE") {
		return number, false
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil || parsed != math.Trunc(parsed) || math.Abs(parsed) > 1<<53 {
		return number, false
	}
	return json.Number(strconv.FormatInt(int64(parsed), 10)), true
}

func jsonNumbersToFloat(value any) any {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case map[string]any:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[key] = jsonNumbersToFloat(item)
		}
		return converted
	case []any:
		converted := make([]any, len(typed))
		for index, item := range typed {
			converted[index] = jsonNumbersToFloat(item)
		}
		return converted
	}
	return value
}

func repairResponseJSON(raw []byte, schemas map[string]map[string]any) []byte {
	if !bytes.Contains(raw, []byte(`"function_call"`)) {
		return raw
	}
	var response map[string]any
	if json.Unmarshal(raw, &response) != nil || !repairOutputItems(response, schemas, true) {
		return raw
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return raw
	}
	return encoded
}

func repairOutputItems(response map[string]any, schemas map[string]map[string]any, report bool) bool {
	changed := false
	for _, item := range responses.InterfaceMaps(response["output"]) {
		changed = repairFunctionItem(item, schemas, report) || changed
	}
	return changed
}

func repairFunctionItem(item map[string]any, schemas map[string]map[string]any, report bool) bool {
	if item["type"] != "function_call" {
		return false
	}
	name, _ := item["name"].(string)
	arguments, ok := item["arguments"].(string)
	schema := schemas[name]
	if !ok || schema == nil {
		return false
	}
	repaired := repairFunctionArguments(name, arguments, schema, report)
	if repaired == arguments {
		return false
	}
	item["arguments"] = repaired
	return true
}

type heldFunctionCall struct {
	name        string
	arguments   strings.Builder
	lastDelta   map[string]any
	passthrough bool
	released    bool
}

// toolArgumentsRepairStream rewrites only function-call argument events of a
// Build SSE stream. Every other frame is forwarded byte for byte, so a frame is
// decoded only when it mentions a function call.
type toolArgumentsRepairStream struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	schemas map[string]map[string]any
	calls   map[string]*heldFunctionCall
	order   []*heldFunctionCall
	held    int
	frame   []byte
	out     bytes.Buffer
	err     error
}

func newToolArgumentsRepairStream(body io.ReadCloser, schemas map[string]map[string]any) io.ReadCloser {
	return &toolArgumentsRepairStream{source: body, reader: bufio.NewReaderSize(body, 32<<10), schemas: schemas, calls: map[string]*heldFunctionCall{}}
}

func (s *toolArgumentsRepairStream) Read(p []byte) (int, error) {
	for s.out.Len() == 0 && s.err == nil {
		s.readFrame()
	}
	if s.out.Len() > 0 {
		return s.out.Read(p)
	}
	return 0, s.err
}

func (s *toolArgumentsRepairStream) Close() error { return s.source.Close() }

func (s *toolArgumentsRepairStream) readFrame() {
	line, err := s.reader.ReadSlice('\n')
	for err == bufio.ErrBufferFull {
		s.frame = append(s.frame, line...)
		line, err = s.reader.ReadSlice('\n')
	}
	s.frame = append(s.frame, line...)
	if err != nil {
		// Held arguments are released before the stream ends, and a trailing
		// partial frame is forwarded as-is.
		s.releaseAll()
		s.out.Write(s.frame)
		s.frame = s.frame[:0]
		s.err = err
		return
	}
	if len(bytes.TrimRight(line, "\r\n")) != 0 {
		return
	}
	frame := s.frame
	s.frame = s.frame[:0]
	if !bytes.Contains(frame, []byte("function_call")) {
		s.out.Write(frame)
		return
	}
	s.rewriteFrame(frame)
}

func (s *toolArgumentsRepairStream) rewriteFrame(frame []byte) {
	event, data := parseSSEFrame(frame)
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		s.out.Write(frame)
		return
	}
	kind, _ := payload["type"].(string)
	if kind == "" {
		kind = event
	}
	switch kind {
	case "response.output_item.added":
		if item, _ := payload["item"].(map[string]any); item["type"] == "function_call" {
			id, _ := item["id"].(string)
			name, _ := item["name"].(string)
			if s.schemas[name] != nil && id != "" {
				call := &heldFunctionCall{name: name}
				s.calls[id] = call
				s.order = append(s.order, call)
			}
		}
	case "response.function_call_arguments.delta":
		id, _ := payload["item_id"].(string)
		call := s.calls[id]
		if call == nil || call.passthrough || call.released {
			break
		}
		delta, _ := payload["delta"].(string)
		if call.arguments.Len()+len(delta) > maxHeldFunctionArgumentsBytes || s.held+len(delta) > maxTotalHeldFunctionArgsBytes {
			// Too large to hold: release what is held and stream the rest raw.
			call.passthrough = true
			s.held -= call.arguments.Len()
			if call.arguments.Len() > 0 {
				flushed := cloneEventPayload(payload)
				flushed["delta"] = call.arguments.String()
				s.writeEvent(event, flushed)
				call.arguments.Reset()
			}
			break
		}
		call.arguments.WriteString(delta)
		s.held += len(delta)
		call.lastDelta = payload
		return
	case "response.function_call_arguments.done":
		id, _ := payload["item_id"].(string)
		call := s.calls[id]
		if call == nil {
			break
		}
		arguments, _ := payload["arguments"].(string)
		payload["arguments"] = s.release(call, arguments)
		s.writeEvent(event, payload)
		return
	case "response.output_item.done":
		item, _ := payload["item"].(map[string]any)
		id, _ := item["id"].(string)
		if call := s.calls[id]; call != nil {
			arguments, _ := item["arguments"].(string)
			s.release(call, arguments)
		}
		if repairFunctionItem(item, s.schemas, false) {
			s.writeEvent(event, payload)
			return
		}
	case "response.completed", "response.incomplete", "response.failed":
		s.releaseAll()
		if response, _ := payload["response"].(map[string]any); repairOutputItems(response, s.schemas, false) {
			s.writeEvent(event, payload)
			return
		}
	}
	s.out.Write(frame)
}

// release delivers a call's held argument deltas as one repaired delta and
// returns the repaired arguments. complete, when non-empty, is the upstream's
// own final argument string and wins over the concatenated deltas.
func (s *toolArgumentsRepairStream) release(call *heldFunctionCall, complete string) string {
	arguments := complete
	if arguments == "" {
		arguments = call.arguments.String()
	}
	if call.released {
		return repairFunctionArguments(call.name, arguments, s.schemas[call.name], false)
	}
	call.released = true
	repaired := repairFunctionArguments(call.name, arguments, s.schemas[call.name], true)
	if !call.passthrough && call.lastDelta != nil {
		call.lastDelta["delta"] = repaired
		s.writeEvent("response.function_call_arguments.delta", call.lastDelta)
	}
	s.held -= call.arguments.Len()
	call.arguments.Reset()
	call.lastDelta = nil
	return repaired
}

func (s *toolArgumentsRepairStream) releaseAll() {
	for _, call := range s.order {
		if !call.released && call.lastDelta != nil {
			s.release(call, "")
		}
	}
}

func (s *toolArgumentsRepairStream) writeEvent(event string, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if event != "" {
		s.out.WriteString("event: ")
		s.out.WriteString(event)
		s.out.WriteByte('\n')
	}
	s.out.WriteString("data: ")
	s.out.Write(encoded)
	s.out.WriteString("\n\n")
}

func parseSSEFrame(frame []byte) (event string, data []byte) {
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if value, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			event = string(bytes.TrimSpace(value))
		} else if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}
	}
	return event, data
}

func cloneEventPayload(payload map[string]any) map[string]any {
	clone := make(map[string]any, len(payload))
	maps.Copy(clone, payload)
	return clone
}
