package adapter

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"encoding/json"
)

type openAIMessageStartPayload struct {
	Message *struct {
		Model string `json:"model"`
	} `json:"message"`
}

type openAIContentBlockStartPayload struct {
	Index        int `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
		ID   string `json:"id,omitempty"`
		Name string `json:"name,omitempty"`
	} `json:"content_block"`
}

type openAIContentBlockDeltaPayload struct {
	Index int `json:"index"`
	Delta *struct {
		Type        string  `json:"type"`
		Text        *string `json:"text,omitempty"`
		PartialJSON *string `json:"partial_json,omitempty"`
		Thinking    *string `json:"thinking,omitempty"`
	} `json:"delta"`
}

type openAIMessageDeltaPayload struct {
	Delta *struct {
		StopReason *string `json:"stop_reason,omitempty"`
	} `json:"delta"`
}

const (
	openAIChunkIDPrefix        = "{\"id\":"
	openAIChunkObjectCreated   = ",\"object\":\"chat.completion.chunk\",\"created\":"
	openAIChunkModelPrefix     = ",\"model\":"
	openAIChunkChoicesPrefix   = ",\"choices\":[{\"index\":0,"
	openAIMessageStartSuffix   = "\"delta\":{\"role\":\"assistant\"}}]}"
	openAITextDeltaPrefix      = "\"delta\":{\"content\":"
	openAIContentDeltaSuffix   = "}}]}"
	openAIThinkingDeltaPrefix  = "\"delta\":{\"reasoning_content\":"
	openAIToolIndexPrefix      = "\"delta\":{\"tool_calls\":[{\"index\":"
	openAIToolArgsDeltaPrefix  = ",\"function\":{\"arguments\":"
	openAIToolArgsDeltaSuffix  = "}}]}}]}"
	openAIToolStartDeltaPrefix = ",\"id\":"
	openAIToolStartNamePrefix  = ",\"type\":\"function\",\"function\":{\"name\":"
	openAIToolStartDeltaSuffix = ",\"arguments\":\"\"}}]}}]}"
	openAIMessageDeltaPrefix   = "\"delta\":{},\"finish_reason\":"
	openAIMessageDeltaSuffix   = "}]}"
)

func appendOpenAIJSONString(dst []byte, value string) []byte {
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b < 0x20 || b == '\\' || b == '"' || b >= utf8.RuneSelf {
			quoted, _ := json.Marshal(value)
			return append(dst, quoted...)
		}
	}
	dst = append(dst, '"')
	dst = append(dst, value...)
	dst = append(dst, '"')
	return dst
}

func ensureOpenAIChunkCapacity(dst []byte, extra int) []byte {
	if extra <= 0 || cap(dst)-len(dst) >= extra {
		return dst
	}
	grown := make([]byte, len(dst), len(dst)+extra)
	copy(grown, dst)
	return grown
}

func decimalLenInt64(v int64) int {
	if v == 0 {
		return 1
	}
	n := 0
	if v < 0 {
		n++
		if v == -1<<63 {
			return 20
		}
		v = -v
	}
	for v > 0 {
		n++
		v /= 10
	}
	return n
}

var (
	openAIModelMarker           = []byte("\"model\":")
	openAIToolUseTypeMarker     = []byte("\"type\":\"tool_use\"")
	openAITextContentTypeMarker = []byte("\"type\":\"text\"")
	openAITextDeltaMarker       = []byte("\"type\":\"text_delta\"")
	openAIInputJSONMarker       = []byte("\"type\":\"input_json_delta\"")
	openAIThinkingDeltaMarker   = []byte("\"type\":\"thinking_delta\"")
	openAIStopReasonMarker      = []byte("\"stop_reason\":")
	openAITextMarker            = []byte("\"text\":")
	openAIPartialJSONMarker     = []byte("\"partial_json\":")
	openAIThinkingMarker        = []byte("\"thinking\":")
	openAIIDMarker              = []byte("\"id\":")
	openAINameMarker            = []byte("\"name\":")
	openAIStopQuoted            = []byte("\"stop\"")
	openAIToolCallsQuoted       = []byte("\"tool_calls\"")
	openAILengthQuoted          = []byte("\"length\"")
	openAIContentFilterQuoted   = []byte("\"content_filter\"")
	openAIToolUseQuoted         = []byte("\"tool_use\"")
	openAIEndTurnQuoted         = []byte("\"end_turn\"")
	openAIMaxTokensQuoted       = []byte("\"max_tokens\"")
	openAIRefusalQuoted         = []byte("\"refusal\"")
)

func normalizeOpenAIStopReasonQuoted(quotedStopReason []byte) []byte {
	switch {
	case len(quotedStopReason) == 0,
		bytes.Equal(quotedStopReason, []byte(`""`)),
		bytes.Equal(quotedStopReason, openAIStopQuoted),
		bytes.Equal(quotedStopReason, openAIEndTurnQuoted):
		return openAIStopQuoted
	case bytes.Equal(quotedStopReason, openAIToolUseQuoted):
		return openAIToolCallsQuoted
	case bytes.Equal(quotedStopReason, openAIMaxTokensQuoted):
		return openAILengthQuoted
	case bytes.Equal(quotedStopReason, openAIRefusalQuoted):
		return openAIContentFilterQuoted
	default:
		return quotedStopReason
	}
}

func normalizeOpenAIStopReason(stopReason string) string {
	switch strings.TrimSpace(stopReason) {
	case "", "stop", "end_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return strings.TrimSpace(stopReason)
	}
}

func extractJSONStringValueAfter(data []byte, marker []byte) ([]byte, bool) {
	idx := bytes.Index(data, marker)
	if idx < 0 {
		return nil, false
	}
	idx += len(marker)
	for idx < len(data) {
		switch data[idx] {
		case ' ', '\t', '\n', '\r':
			idx++
			continue
		}
		break
	}
	if idx >= len(data) || data[idx] != '"' {
		return nil, false
	}
	start := idx
	idx++
	escaped := false
	for idx < len(data) {
		if escaped {
			escaped = false
			idx++
			continue
		}
		switch data[idx] {
		case '\\':
			escaped = true
		case '"':
			return data[start : idx+1], true
		}
		idx++
	}
	return nil, false
}

func estimatedOpenAIChunkPrefixLen(msgID string, created int64, quotedModel []byte) int {
	modelLen := len(quotedModel)
	if modelLen == 0 {
		modelLen = 2
	}
	return len(openAIChunkIDPrefix) + len(msgID) + 2 +
		len(openAIChunkObjectCreated) + decimalLenInt64(created) +
		len(openAIChunkModelPrefix) + modelLen +
		len(openAIChunkChoicesPrefix)
}

func appendOpenAIChunkPrefix(dst []byte, msgID string, created int64, quotedModel []byte) []byte {
	dst = appendOpenAIJSONString(append(dst, openAIChunkIDPrefix...), msgID)
	dst = append(dst, openAIChunkObjectCreated...)
	dst = strconv.AppendInt(dst, created, 10)
	dst = append(dst, openAIChunkModelPrefix...)
	if len(quotedModel) > 0 {
		dst = append(dst, quotedModel...)
	} else {
		dst = append(dst, '"', '"')
	}
	return append(dst, openAIChunkChoicesPrefix...)
}

func appendOpenAIChunkMessageStart(dst []byte, msgID string, created int64, quotedModel []byte) ([]byte, bool) {
	dst = ensureOpenAIChunkCapacity(dst, estimatedOpenAIChunkPrefixLen(msgID, created, quotedModel)+len(openAIMessageStartSuffix))
	dst = appendOpenAIChunkPrefix(dst, msgID, created, quotedModel)
	dst = append(dst, openAIMessageStartSuffix...)
	return dst, true
}

func appendOpenAIChunkField(dst []byte, msgID string, created int64, field, suffix string, quoted []byte) ([]byte, bool) {
	dst = ensureOpenAIChunkCapacity(dst, estimatedOpenAIChunkPrefixLen(msgID, created, nil)+len(field)+len(quoted)+len(suffix))
	dst = appendOpenAIChunkPrefix(dst, msgID, created, nil)
	dst = append(dst, field...)
	dst = append(dst, quoted...)
	return append(dst, suffix...), true
}

func appendOpenAIChunkToolArgs(dst []byte, msgID string, created int64, index int, quotedArgs []byte) ([]byte, bool) {
	dst = ensureOpenAIChunkCapacity(dst, estimatedOpenAIChunkPrefixLen(msgID, created, nil)+len(openAIToolIndexPrefix)+decimalLenInt64(int64(index))+len(openAIToolArgsDeltaPrefix)+len(quotedArgs)+len(openAIToolArgsDeltaSuffix))
	dst = appendOpenAIChunkPrefix(dst, msgID, created, nil)
	dst = append(dst, openAIToolIndexPrefix...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, openAIToolArgsDeltaPrefix...)
	dst = append(dst, quotedArgs...)
	dst = append(dst, openAIToolArgsDeltaSuffix...)
	return dst, true
}

func appendOpenAIChunkToolStart(dst []byte, msgID string, created int64, index int, quotedID []byte, quotedName []byte) ([]byte, bool) {
	dst = ensureOpenAIChunkCapacity(dst, estimatedOpenAIChunkPrefixLen(msgID, created, nil)+len(openAIToolIndexPrefix)+decimalLenInt64(int64(index))+len(openAIToolStartDeltaPrefix)+len(quotedID)+len(openAIToolStartNamePrefix)+len(quotedName)+len(openAIToolStartDeltaSuffix))
	dst = appendOpenAIChunkPrefix(dst, msgID, created, nil)
	dst = append(dst, openAIToolIndexPrefix...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	if len(quotedID) != 0 {
		dst = append(dst, openAIToolStartDeltaPrefix...)
		dst = append(dst, quotedID...)
	}
	dst = append(dst, openAIToolStartNamePrefix...)
	dst = append(dst, quotedName...)
	dst = append(dst, openAIToolStartDeltaSuffix...)
	return dst, true
}

func appendOpenAIChunkMessageDelta(dst []byte, msgID string, created int64, quotedStopReason []byte, usage map[string]any) ([]byte, bool) {
	raw, _ := appendOpenAIChunkField(dst, msgID, created, openAIMessageDeltaPrefix, openAIMessageDeltaSuffix, quotedStopReason)
	return appendOpenAIChunkUsage(raw, usage), true
}

func appendOpenAIChunkFast(dst []byte, msgID string, created int64, event string, data []byte) ([]byte, bool) {
	switch event {
	case "message_start":
		if quotedModel, ok := extractJSONStringValueAfter(data, openAIModelMarker); ok {
			return appendOpenAIChunkMessageStart(dst, msgID, created, quotedModel)
		}
	case "content_block_start":
		if bytes.Contains(data, openAIToolUseTypeMarker) {
			quotedID, okID := extractJSONStringValueAfter(data, openAIIDMarker)
			quotedName, okName := extractJSONStringValueAfter(data, openAINameMarker)
			if okID && okName {
				index, ok := openAIToolIndex(data)
				if !ok {
					return nil, false
				}
				return appendOpenAIChunkToolStart(dst, msgID, created, index, quotedID, quotedName)
			}
		}
		if bytes.Contains(data, openAITextContentTypeMarker) {
			if quotedText, ok := extractJSONStringValueAfter(data, openAITextMarker); ok && len(quotedText) > 2 {
				return appendOpenAIChunkField(dst, msgID, created, openAITextDeltaPrefix, openAIContentDeltaSuffix, quotedText)
			}
		}
	case "content_block_delta":
		switch {
		case bytes.Contains(data, openAITextDeltaMarker):
			if quotedText, ok := extractJSONStringValueAfter(data, openAITextMarker); ok {
				return appendOpenAIChunkField(dst, msgID, created, openAITextDeltaPrefix, openAIContentDeltaSuffix, quotedText)
			}
		case bytes.Contains(data, openAIInputJSONMarker):
			if quotedArgs, ok := extractJSONStringValueAfter(data, openAIPartialJSONMarker); ok {
				index, ok := openAIToolIndex(data)
				if !ok {
					return nil, false
				}
				return appendOpenAIChunkToolArgs(dst, msgID, created, index, quotedArgs)
			}
		case bytes.Contains(data, openAIThinkingDeltaMarker):
			if quotedThinking, ok := extractJSONStringValueAfter(data, openAIThinkingMarker); ok {
				return appendOpenAIChunkField(dst, msgID, created, openAIThinkingDeltaPrefix, openAIContentDeltaSuffix, quotedThinking)
			}
		}
	case "message_delta":
		if quotedStopReason, ok := extractJSONStringValueAfter(data, openAIStopReasonMarker); ok {
			var usage map[string]any
			if bytes.Contains(data, []byte(`"usage"`)) {
				usage = messageDeltaChatUsage(data)
			}
			return appendOpenAIChunkMessageDelta(dst, msgID, created, normalizeOpenAIStopReasonQuoted(quotedStopReason), usage)
		}
	case "message_stop":
		return nil, false
	}
	return nil, false
}

func openAIToolIndex(data []byte) (int, bool) {
	var payload struct {
		Index int `json:"index"`
	}
	if json.Unmarshal(data, &payload) != nil || payload.Index < 0 {
		return 0, false
	}
	return payload.Index, true
}

// appendOpenAIChunkSlow handles valid JSON that does not have the gateway's
// compact field spelling. Both decoders use the same output encoder.
func appendOpenAIChunkSlow(dst []byte, msgID string, created int64, event string, data []byte) ([]byte, bool) {
	quote := func(value string) []byte {
		raw, _ := json.Marshal(value)
		return raw
	}
	switch event {
	case "message_start":
		var payload openAIMessageStartPayload
		if json.Unmarshal(data, &payload) == nil && payload.Message != nil {
			return appendOpenAIChunkMessageStart(dst, msgID, created, quote(payload.Message.Model))
		}
	case "content_block_start":
		var payload openAIContentBlockStartPayload
		if json.Unmarshal(data, &payload) != nil || payload.ContentBlock == nil || payload.Index < 0 {
			return nil, false
		}
		block := payload.ContentBlock
		if block.Type == "text" && block.Text != "" {
			return appendOpenAIChunkField(dst, msgID, created, openAITextDeltaPrefix, openAIContentDeltaSuffix, quote(block.Text))
		}
		if block.Type == "tool_use" {
			var id []byte
			if block.ID != "" {
				id = quote(block.ID)
			}
			return appendOpenAIChunkToolStart(dst, msgID, created, payload.Index, id, quote(block.Name))
		}
	case "content_block_delta":
		var payload openAIContentBlockDeltaPayload
		if json.Unmarshal(data, &payload) != nil || payload.Delta == nil || payload.Index < 0 {
			return nil, false
		}
		delta := payload.Delta
		switch {
		case delta.Type == "text_delta" && delta.Text != nil:
			return appendOpenAIChunkField(dst, msgID, created, openAITextDeltaPrefix, openAIContentDeltaSuffix, quote(*delta.Text))
		case delta.Type == "thinking_delta" && delta.Thinking != nil:
			return appendOpenAIChunkField(dst, msgID, created, openAIThinkingDeltaPrefix, openAIContentDeltaSuffix, quote(*delta.Thinking))
		case delta.Type == "input_json_delta" && delta.PartialJSON != nil:
			return appendOpenAIChunkToolArgs(dst, msgID, created, payload.Index, quote(*delta.PartialJSON))
		}
	case "message_delta":
		var payload openAIMessageDeltaPayload
		if json.Unmarshal(data, &payload) == nil && payload.Delta != nil && payload.Delta.StopReason != nil {
			return appendOpenAIChunkMessageDelta(dst, msgID, created, quote(normalizeOpenAIStopReason(*payload.Delta.StopReason)), messageDeltaChatUsage(data))
		}
	}
	return nil, false
}

func AppendOpenAIChunk(dst []byte, msgID string, created int64, event string, data []byte) ([]byte, bool) {
	if raw, ok := appendOpenAIChunkFast(dst, msgID, created, event, data); ok {
		return raw, true
	}
	return appendOpenAIChunkSlow(dst, msgID, created, event, data)
}
