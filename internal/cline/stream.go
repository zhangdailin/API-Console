package cline

import (
	"bufio"
	"fmt"
	"html"
	"io"
	"regexp"
	"strings"

	"encoding/json"

	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// streamResult accumulates what the upstream produced so the caller can decide
// whether the attempt was meaningful and which stop reason to report.
type streamResult struct {
	SawMeaningfulEvent   bool
	ToolCallCount        int
	UpstreamFinishReason string
	Usage                map[string]interface{}
	// ThinkingSignature is one stable signature for every reasoning delta of
	// the stream, generated on first sight so a multi-delta block stays signed.
	ThinkingSignature string
}

// FinishReason maps the accumulated stream onto an Anthropic-style stop reason.
func (r streamResult) FinishReason() string {
	if r.ToolCallCount > 0 {
		return "tool_use"
	}
	switch strings.ToLower(strings.TrimSpace(r.UpstreamFinishReason)) {
	case "length", "max_tokens", "max_token_limit":
		return "max_tokens"
	case "stop", "stop_sequence", "end_turn", "":
		return "end_turn"
	default:
		return r.UpstreamFinishReason
	}
}

// NewToolCallID mints a local tool-call id for upstream deltas that omit one.
func NewToolCallID() string { return util.NewToolCallID() }

// newThinkingSignature mints the per-stream thinking signature the Anthropic
// surface attaches to a thinking block. The prefix names the channel so a
// signature can be attributed when it round-trips in history replay.
func newThinkingSignature() string { return util.NewThinkingSignature("cline-v1") }

// streamChunk is one `data:` line of the SSE response.
type streamChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			// GLM-compatible gateways have emitted the same channel under
			// `reasoning` or `thinking`; keep all aliases so the reasoning block
			// is not silently lost at the Cline boundary.
			Reasoning string `json:"reasoning"`
			Thinking  string `json:"thinking"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage map[string]interface{} `json:"usage"`
	Error struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
	} `json:"error"`
}

var clineTextToolBlockRE = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
var clineTextToolArgumentRE = regexp.MustCompile(`(?s)<arg_key>\s*([^<]+?)\s*</arg_key>\s*<arg_value>\s*(.*?)\s*</arg_value>`)
var clineTextToolNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// clineFunctionToolBlockRE matches the `<function=NAME>` dialect:
//
//	<tool_call><function=write_stdin><parameter=chars>x</parameter>…</function></tool_call>
//
// mimo-v2.6-flash serves this through the Xiaomi provider. It is a different
// shape from the GLM dialects below, so it is tried first.
var clineFunctionToolBlockRE = regexp.MustCompile(`(?s)<tool_call>\s*<function=([^>]+)>(.*?)</function>\s*</tool_call>`)
var clineFunctionParameterRE = regexp.MustCompile(`(?s)<parameter=([^>]+)>(.*?)</parameter>`)

const maxClineTextToolBufferBytes = 1 << 20
const clineTextToolOpenTag = "<tool_call>"
const clineTextToolCloseTag = "</tool_call>"

// parseClineTextToolCalls converts the textual fallbacks emitted by some Cline
// models (notably z-ai/glm) when the gateway returns a tool call inside the text
// delta instead of OpenAI tool_calls deltas. Three formats have been observed:
//
//   - <tool_call><function=NAME><parameter=KEY>VALUE</parameter>…</function></tool_call>
//   - <tool_call>bash:ignored</arg_value><arg_key>command</arg_key>…</tool_call>
//   - <tool_call>glob<tool_call>glob: *<tool_call>args: {"pattern":"*"}...
//
// The second format's value before arg_key is an unlabelled duplicate and is
// intentionally ignored.
func parseClineTextToolCalls(text string) (string, []toolCall) {
	visible, calls := parseClineFunctionToolCalls(text)
	if len(calls) > 0 {
		return visible, calls
	}
	visible, calls = parseClineClosedTextToolCalls(text)
	if len(calls) > 0 {
		return visible, calls
	}
	return parseClineRepeatedTagToolCall(text)
}

// parseClineFunctionToolCalls reads the `<function=NAME>` dialect, where every
// parameter is a labelled element carrying raw text rather than JSON. A value
// that parses as JSON keeps its type, so `2000` stays a number and not "2000".
//
// A block is only recognised once it is complete. The upstream streams one block
// across several frames, so a partial block is left in the text rather than
// being parsed into a call with half its parameters.
func parseClineFunctionToolCalls(text string) (string, []toolCall) {
	matches := clineFunctionToolBlockRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	calls := make([]toolCall, 0, len(matches))
	var visible strings.Builder
	last := 0
	for _, match := range matches {
		if len(match) < 6 {
			continue
		}
		name := strings.TrimSpace(html.UnescapeString(text[match[2]:match[3]]))
		args := map[string]interface{}{}
		for _, parameter := range clineFunctionParameterRE.FindAllStringSubmatch(text[match[4]:match[5]], -1) {
			if len(parameter) != 3 {
				continue
			}
			key := strings.TrimSpace(html.UnescapeString(parameter[1]))
			if key == "" {
				continue
			}
			value := strings.TrimSpace(html.UnescapeString(parameter[2]))
			var typed interface{}
			if json.Unmarshal([]byte(value), &typed) == nil {
				args[key] = typed
			} else {
				args[key] = value
			}
		}
		if name == "" || len(args) == 0 {
			visible.WriteString(text[last:match[1]])
			last = match[1]
			continue
		}
		raw, err := json.Marshal(args)
		if err != nil {
			visible.WriteString(text[last:match[1]])
			last = match[1]
			continue
		}
		visible.WriteString(text[last:match[0]])
		last = match[1]
		calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: string(raw)}})
	}
	visible.WriteString(text[last:])
	return visible.String(), calls
}

func parseClineClosedTextToolCalls(text string) (string, []toolCall) {
	matches := clineTextToolBlockRE.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	calls := make([]toolCall, 0, len(matches))
	var visible strings.Builder
	last := 0
	for _, match := range matches {
		if len(match) < 4 {
			continue
		}
		visible.WriteString(text[last:match[0]])
		body := text[match[2]:match[3]]
		colon := strings.IndexByte(body, ':')
		if colon <= 0 {
			visible.WriteString(text[match[0]:match[1]])
			last = match[1]
			continue
		}
		name := strings.TrimSpace(body[:colon])
		args := map[string]interface{}{}
		for _, arg := range clineTextToolArgumentRE.FindAllStringSubmatch(body[colon+1:], -1) {
			if len(arg) != 3 {
				continue
			}
			key := strings.TrimSpace(html.UnescapeString(arg[1]))
			value := html.UnescapeString(strings.TrimSpace(arg[2]))
			var typed interface{}
			if json.Unmarshal([]byte(value), &typed) == nil {
				args[key] = typed
			} else {
				args[key] = value
			}
		}
		if name == "" || len(args) == 0 {
			visible.WriteString(text[match[0]:match[1]])
			last = match[1]
			continue
		}
		raw, err := json.Marshal(args)
		if err != nil {
			visible.WriteString(text[match[0]:match[1]])
			last = match[1]
			continue
		}
		calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: string(raw)}})
		last = match[1]
	}
	visible.WriteString(text[last:])
	return visible.String(), calls
}

func parseClineRepeatedTagToolCall(text string) (string, []toolCall) {
	const marker = clineTextToolOpenTag
	first := strings.Index(text, marker)
	if first < 0 {
		return text, nil
	}
	segments := strings.Split(text[first+len(marker):], marker)
	if len(segments) < 1 {
		return text, nil
	}
	calls := make([]toolCall, 0, len(segments))
	for index := 0; index < len(segments); {
		segment := strings.TrimSpace(segments[index])
		// Compact GLM fallback: <tool_call>glob,{"pattern":"*"}
		if comma := strings.IndexByte(segment, ','); comma > 0 {
			name := strings.TrimSpace(segment[:comma])
			arguments := strings.TrimSpace(segment[comma+1:])
			if clineTextToolNameRE.MatchString(name) && json.Valid([]byte(arguments)) {
				calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: arguments}})
				index++
				continue
			}
		}
		if !clineTextToolNameRE.MatchString(segment) {
			index++
			continue
		}
		name := segment
		args := map[string]interface{}{}
		cursor := index + 1
		for ; cursor < len(segments); cursor++ {
			part := strings.TrimSpace(segments[cursor])
			if clineTextToolNameRE.MatchString(part) {
				break
			}
			colon := strings.IndexByte(part, ':')
			if colon <= 0 {
				continue
			}
			key := strings.TrimSpace(part[:colon])
			value := strings.TrimSpace(part[colon+1:])
			if key == "" || value == "" || strings.EqualFold(key, name) || strings.EqualFold(key, "run_in_background") {
				continue
			}
			if strings.EqualFold(key, "args") || strings.EqualFold(key, "arguments") {
				var object map[string]interface{}
				if json.Unmarshal([]byte(value), &object) == nil {
					for objectKey, objectValue := range object {
						args[objectKey] = objectValue
					}
				}
				continue
			}
			var typed interface{}
			if json.Unmarshal([]byte(value), &typed) == nil {
				args[key] = typed
			} else {
				args[key] = value
			}
		}
		if len(args) > 0 {
			raw, err := json.Marshal(args)
			if err == nil {
				calls = append(calls, toolCall{ID: NewToolCallID(), Type: "function", Function: toolCallFunction{Name: name, Arguments: string(raw)}})
			}
		}
		if cursor <= index {
			index++
		} else {
			index = cursor
		}
	}
	if len(calls) == 0 {
		return text, nil
	}
	return text[:first], calls
}

// clineThinkingSplitter separates GLM-style in-content <think> blocks from
// answer text, including tags split across SSE frames. Unmatched tag prefixes
// are held for at most len("</think>") bytes and flushed at end of stream.
// A complete opening tag is required before treating anything as reasoning.
type clineThinkingSplitter struct {
	thinking bool
	pending  string
}

func (s *clineThinkingSplitter) feed(content string, final bool, emitReasoning, emitText func(string)) {
	s.pending += content
	for len(s.pending) > 0 {
		tag := "<think>"
		emit := emitText
		if s.thinking {
			tag = "</think>"
			emit = emitReasoning
		}
		if idx := strings.Index(s.pending, tag); idx >= 0 {
			emit(s.pending[:idx])
			s.pending = s.pending[idx+len(tag):]
			s.thinking = !s.thinking
			continue
		}
		// Retain only the longest suffix that could be the beginning of a
		// delimiter; everything else is safe to emit right now.
		keep := 0
		if !final {
			for n := min(len(s.pending), len(tag)-1); n > 0; n-- {
				if strings.HasSuffix(s.pending, tag[:n]) {
					keep = n
					break
				}
			}
		}
		emit(s.pending[:len(s.pending)-keep])
		s.pending = s.pending[len(s.pending)-keep:]
		break
	}
}

// consumeStream parses the SSE body and forwards deltas to the caller.
func consumeStream(body io.Reader, toolsEnabled bool, onMessage func(upstream.SSEMessage)) (streamResult, error) {
	scanner := bufio.NewScanner(body)
	buffer := util.AcquireStreamBuffer()
	defer util.ReleaseStreamBuffer(buffer)
	scanner.Buffer(buffer[:], 8*1024*1024)
	result := streamResult{}
	tools := util.NewToolCallAccumulator()
	var pendingText strings.Builder
	var thinkingSplitter clineThinkingSplitter
	sawFinish := false

	emitText := func(text string) { upstream.EmitTextDelta(onMessage, text, &result.SawMeaningfulEvent) }

	emitReasoning := func(reasoning string) {
		if reasoning == "" {
			return
		}
		result.SawMeaningfulEvent = true
		if onMessage != nil {
			// One signature per stream also covers reasoning embedded in content.
			if result.ThinkingSignature == "" {
				result.ThinkingSignature = newThinkingSignature()
			}
			onMessage(upstream.SSEMessage{Type: "model.reasoning-delta", Event: map[string]interface{}{
				"delta":     reasoning,
				"signature": result.ThinkingSignature,
			}})
		}
	}

	// A native tool call whose arguments never closed is the upstream emitting
	// the same call twice: the deltas stop mid-object while the textual copy in
	// the content delta carries every parameter. Repairing the native call keeps
	// one tool_use block with the id the deltas carried and complete arguments;
	// emitting both would hand the client a malformed fragment plus a duplicate.
	// A recovered call that matches nothing is a genuine second intent, so it is
	// appended rather than dropped.
	recoverTextToolCalls := func(calls []toolCall) {
		for _, call := range calls {
			if tools.RepairArguments(call.Function.Name, call.Function.Arguments) {
				continue
			}
			if tools.HasCallNamed(call.Function.Name) {
				continue
			}
			tools.Add(tools.Order(), call.ID, call.Function.Name, call.Function.Arguments)
		}
	}

	// flushPendingText emits the part of the buffered content that can no longer
	// become a tool call and keeps the rest. A textual fallback block arrives in
	// fragments, so a complete block is resolved as soon as it closes and only
	// an unclosed tail is held. `final` releases the tail: at end of stream an
	// incomplete opening tag is prose, and holding it would lose the text.
	//
	// Holding only the unclosed tail is what keeps an answer streaming while a
	// block assembles, and it is why the repair below can still see the block:
	// the closing tag arrives before the finish_reason that triggers the flush.
	flushPendingText := func(final bool) {
		if pendingText.Len() == 0 {
			return
		}
		buffer := pendingText.String()
		// Resolve every complete block, keeping the text before and after it.
		visible, calls := parseClineTextToolCalls(buffer)
		recoverTextToolCalls(calls)
		if final {
			pendingText.Reset()
			emitText(visible)
			return
		}
		// Hold the unclosed tail: everything from the last opening tag that has
		// no closing tag yet, plus a trailing fragment that could still open one.
		keep := 0
		if start := strings.LastIndex(visible, clineTextToolOpenTag); start >= 0 &&
			!strings.Contains(visible[start:], clineTextToolCloseTag) {
			keep = len(visible) - start
		} else {
			for n := min(len(visible), len(clineTextToolOpenTag)-1); n > 0; n-- {
				if strings.HasSuffix(visible, clineTextToolOpenTag[:n]) {
					keep = n
					break
				}
			}
		}
		if len(visible)-keep <= 0 {
			pendingText.Reset()
			pendingText.WriteString(visible)
			return
		}
		emitText(visible[:len(visible)-keep])
		pendingText.Reset()
		pendingText.WriteString(visible[len(visible)-keep:])
	}

	emitContent := func(content string) {
		if content == "" {
			return
		}
		if !toolsEnabled {
			// Without declared tools, literal tool markup is user-visible content.
			emitText(content)
			return
		}
		pendingText.WriteString(content)
		// A buffer this large is not a tool call; release it rather than grow.
		flushPendingText(pendingText.Len() > maxClineTextToolBufferBytes)
	}

	emitTools := func() {
		upstream.EmitToolCalls(onMessage, tools.CompleteAll(), &result.SawMeaningfulEvent, &result.ToolCallCount)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			sawFinish = true
			break
		}
		if payload == "" {
			continue
		}

		// Some chunks arrive wrapped in {"data":{...}}; the delta has to be read
		// out of the envelope before it can be decoded as a chunk.
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil || !chunkLooksDecoded(payload) {
			unwrapped, ok := unwrapEnvelope(payload)
			if !ok {
				return result, fmt.Errorf("cline stream protocol error: invalid chunk")
			}
			if err := json.Unmarshal([]byte(unwrapped), &chunk); err != nil || !chunkLooksDecoded(unwrapped) {
				return result, fmt.Errorf("cline stream protocol error: invalid wrapped chunk")
			}
		}
		if msg := strings.TrimSpace(chunk.Error.Message); msg != "" {
			return result, fmt.Errorf("cline stream error: %s", msg)
		}
		upstream.ApplyStreamUsage(onMessage, upstream.NormalizeUsageMap(chunk.Usage), &result.SawMeaningfulEvent, &result.Usage)
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta

		// Providers are not consistent about the GLM reasoning field name.
		// Normalize aliases before emitting the shared reasoning event.
		reasoning := delta.ReasoningContent
		if reasoning == "" {
			reasoning = delta.Reasoning
		}
		if reasoning == "" {
			reasoning = delta.Thinking
		}
		if reasoning != "" {
			emitReasoning(reasoning)
		}
		if delta.Content != "" {
			thinkingSplitter.feed(delta.Content, false, emitReasoning, emitContent)
		}
		for _, call := range delta.ToolCalls {
			result.SawMeaningfulEvent = true
			tools.Add(call.Index, call.ID, call.Function.Name, call.Function.Arguments)
		}
		// Arguments can span deltas, so the textual fallback is only resolvable
		// once the stream has stopped producing content. Flushing before the
		// finish-time emit lets a truncated native call be repaired in place.
		finishReason := strings.TrimSpace(chunk.Choices[0].FinishReason)
		if finishReason != "" {
			result.UpstreamFinishReason = finishReason
			sawFinish = true
			flushPendingText(true)
			emitTools()
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("failed to read cline stream: %w", err)
	}
	if !sawFinish {
		// EOF before either an explicit finish_reason or [DONE] is a truncated
		// attempt. Returning success would silently accept a partial answer.
		return result, ErrStreamTruncated
	}
	thinkingSplitter.feed("", true, emitReasoning, emitContent)
	flushPendingText(true)
	emitTools()
	return result, nil
}

// chunkLooksDecoded reports whether a payload decoded as a chunk rather than
// merely as JSON. An envelope also decodes into streamChunk — every field is
// missing — so an object carrying only `data` has to be recognized and unwrapped.
func chunkLooksDecoded(payload string) bool {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &probe); err != nil {
		return false
	}
	for _, key := range []string{"choices", "usage", "id", "object", "model"} {
		if _, ok := probe[key]; ok {
			return true
		}
	}
	return false
}

// unwrapEnvelope reads the inner object of a {"data":{...}} chunk.
func unwrapEnvelope(payload string) (string, bool) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &envelope); err != nil {
		return "", false
	}
	inner := strings.TrimSpace(string(envelope.Data))
	if inner == "" || inner[0] != '{' {
		return "", false
	}
	return inner, true
}
