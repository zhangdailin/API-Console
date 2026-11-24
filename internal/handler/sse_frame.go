package handler

import (
	"encoding/binary"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

const (
	jsonHexDigits = "0123456789abcdef"

	// SWAR ("SIMD within a register") constants: eight bytes' worth of 0x01 and
	// of 0x80, the masks the byte-wise bit tricks below are built from.
	swarLo = 0x0101010101010101
	swarHi = 0x8080808080808080
)

var (
	sseMessageStopBytes = []byte(`{"type":"message_stop"}`)
)

// swarHasZero returns a nonzero word iff any of v's eight bytes is zero. The
// zero lane borrows out of its own byte, and the final mask reads that borrow out
// of the 0x80 bit.
func swarHasZero(v uint64) uint64 { return (v - swarLo) & ^v & swarHi }

// swarHasByte returns a nonzero word iff any of w's eight bytes equals c.
func swarHasByte(w uint64, c byte) uint64 { return swarHasZero(w ^ (uint64(c) * swarLo)) }

// The five characters encoding/json escapes beyond the control range are not
// five independent tests' worth of work. Two of them sit exactly one bit away
// from a partner — '"' (0x22) from '&' (0x26) in bit 2, '<' (0x3C) from '>'
// (0x3E) in bit 1 — so masking that bit away turns each pair into a single
// masked-equality test. Only '\\' is left on its own: three predicates where the
// obvious spelling needs five, on a predicate the streaming path runs once per
// eight bytes of every answer.
const (
	swarQuoteAmpMask  = 0xFBFBFBFBFBFBFBFB
	swarQuoteAmpValue = 0x2222222222222222
	swarAngleMask     = 0xFDFDFDFDFDFDFDFD
	swarAngleValue    = 0x3C3C3C3C3C3C3C3C
)

// swarHasMaskedByte returns a nonzero word iff any byte of w matches value on the
// bits mask keeps. With the masks above the only bytes that match are the two
// members of each pair.
func swarHasMaskedByte(w, mask, value uint64) uint64 { return swarHasZero((w & mask) ^ value) }

// load8LE reads eight bytes of a string as one little-endian word. The copy into
// a fixed eight-byte array is not a pessimisation: it compiles to a single wide
// move plus a single wide load, which measured about twice as fast as assembling
// the word from eight indexed byte reads.
func load8LE(s string, i int) uint64 {
	var buf [8]byte
	copy(buf[:], s[i:i+8])
	return binary.LittleEndian.Uint64(buf[:])
}

// jsonRawChunkClean reports whether all eight bytes of w may be copied into a
// JSON string literal verbatim.
//
// Each predicate is an exact "any byte in this set" test over the whole word, so
// a true answer is a licence to skip eight bytes outright and a false answer
// costs only a fallback to the scalar scanner — never correctness.
//
// A byte >= 0x80 is rejected wholesale because it may begin U+2028/U+2029, which
// encoding/json escapes too but which cannot be recognised without decoding the
// rune.
func jsonRawChunkClean(w uint64) bool {
	if w&swarHi != 0 {
		return false
	}
	// hasless(w, 0x20): the standard subtract-and-mask form, exact for thresholds
	// below 0x80.
	if (w-swarLo*0x20)&^w&swarHi != 0 {
		return false
	}
	escapes := swarHasMaskedByte(w, swarQuoteAmpMask, swarQuoteAmpValue) |
		swarHasMaskedByte(w, swarAngleMask, swarAngleValue) |
		swarHasByte(w, '\\')
	return escapes == 0
}

// canAppendJSONRawString reports whether value survives a trip through a JSON
// string literal unchanged, i.e. whether encoding/json would emit it verbatim
// between the quotes.
//
// It is the cheap half of a two-pass encoder: it fails at the first byte that
// needs an escape, so a payload dense in quotes and backslashes — every JSON
// tool argument — is rejected after a single window rather than scanned in full,
// and only a payload that really is clean pays for a complete pass. That pass is
// where the wide scanner earns its keep, and it is the pass the streaming path
// pays for on every text delta.
func canAppendJSONRawString(value string) bool {
	n := len(value)
	i := 0
	for i < n {
		// Eight bytes at a time while the window is provably clean. The leading
		// single-byte test keeps non-ASCII (CJK answers, which are common) out of
		// the wide path entirely rather than paying for a window test that is
		// guaranteed to fail.
		if value[i] < utf8.RuneSelf && i+8 <= n && jsonRawChunkClean(load8LE(value, i)) {
			i += 8
			continue
		}
		b := value[i]
		if b < utf8.RuneSelf {
			if b < 0x20 || b == '\\' || b == '"' || b == '<' || b == '>' || b == '&' {
				return false
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		if r == '\u2028' || r == '\u2029' {
			return false
		}
		i += size
	}
	return true
}

// appendJSONBytes appends value to dst as a quoted JSON string literal.
func appendJSONBytes(dst []byte, value string) ([]byte, error) {
	if canAppendJSONRawString(value) {
		dst = append(dst, '"')
		dst = append(dst, value...)
		dst = append(dst, '"')
		return dst, nil
	}

	originLen := len(dst)
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(value); {
		b := value[i]
		if b < utf8.RuneSelf {
			if b >= 0x20 && b != '\\' && b != '"' && b != '<' && b != '>' && b != '&' {
				i++
				continue
			}
			if start < i {
				dst = append(dst, value[start:i]...)
			}
			switch b {
			case '\\', '"':
				dst = append(dst, '\\', b)
			case '\b':
				dst = append(dst, '\\', 'b')
			case '\f':
				dst = append(dst, '\\', 'f')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0')
				dst = append(dst, jsonHexDigits[b>>4], jsonHexDigits[b&0x0f])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid UTF-8: encoding/json substitutes U+FFFD, and reproducing
			// that here would mean reimplementing the decoder's error policy. Hand
			// the whole value over instead.
			dst = dst[:originLen]
			quoted, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			return append(dst, quoted...), nil
		}
		if r == '\u2028' || r == '\u2029' {
			if start < i {
				dst = append(dst, value[start:i]...)
			}
			if r == '\u2028' {
				dst = append(dst, '\\', 'u', '2', '0', '2', '8')
			} else {
				dst = append(dst, '\\', 'u', '2', '0', '2', '9')
			}
			i += size
			start = i
			continue
		}
		i += size
	}
	if start < len(value) {
		dst = append(dst, value[start:]...)
	}
	dst = append(dst, '"')
	return dst, nil
}

func appendSSEContentBlockStartToolUse(dst []byte, index int, id, name string) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_start","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"content_block":{"type":"tool_use","id":`...)
	var err error
	dst, err = appendJSONBytes(dst, id)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"name":`...)
	dst, err = appendJSONBytes(dst, name)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"input":{}}}`...)
	return dst, nil
}

func appendSSEMessageStart(dst []byte, msgID, model string, inputTokens, outputTokens int) ([]byte, error) {
	dst = append(dst, `{"type":"message_start","message":{"id":`...)
	var err error
	dst, err = appendJSONBytes(dst, msgID)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"type":"message","role":"assistant","content":[],"model":`...)
	dst, err = appendJSONBytes(dst, model)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `,"usage":{"input_tokens":`...)
	dst = strconv.AppendInt(dst, int64(inputTokens), 10)
	dst = append(dst, `,"output_tokens":`...)
	dst = strconv.AppendInt(dst, int64(outputTokens), 10)
	dst = append(dst, `}}}`...)
	return dst, nil
}

func appendSSEContentBlockStartText(dst []byte, index int) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_start","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, `,"content_block":{"type":"text","text":""}}`...)
	return dst, nil
}

func appendSSEContentBlockString(dst []byte, index int, frame, field, value string) ([]byte, error) {
	dst = append(dst, frame...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst, err := appendJSONBytes(append(dst, field...), value)
	if err != nil {
		return nil, err
	}
	return append(dst, '}', '}'), nil
}

const sseDeltaIndex = `{"type":"content_block_delta","index":`

func appendSSEContentBlockStartThinking(dst []byte, index int, signature string) ([]byte, error) {
	return appendSSEContentBlockString(dst, index, `{"type":"content_block_start","index":`, `,"content_block":{"type":"thinking","thinking":"","signature":`, signature)
}

func appendSSEContentBlockDeltaInputJSON(dst []byte, index int, partialJSON string) ([]byte, error) {
	return appendSSEContentBlockString(dst, index, sseDeltaIndex, `,"delta":{"type":"input_json_delta","partial_json":`, partialJSON)
}

func appendSSEContentBlockDeltaText(dst []byte, index int, text string) ([]byte, error) {
	return appendSSEContentBlockString(dst, index, sseDeltaIndex, `,"delta":{"type":"text_delta","text":`, text)
}

func appendSSEContentBlockDeltaThinking(dst []byte, index int, thinking string) ([]byte, error) {
	return appendSSEContentBlockString(dst, index, sseDeltaIndex, `,"delta":{"type":"thinking_delta","thinking":`, thinking)
}

func appendSSEContentBlockStop(dst []byte, index int) ([]byte, error) {
	dst = append(dst, `{"type":"content_block_stop","index":`...)
	dst = strconv.AppendInt(dst, int64(index), 10)
	dst = append(dst, '}')
	return dst, nil
}

func appendSSEMessageDelta(dst []byte, stopReason string, outputTokens int) ([]byte, error) {
	dst = append(dst, `{"type":"message_delta","delta":{"stop_reason":`...)
	var err error
	dst, err = appendJSONBytes(dst, stopReason)
	if err != nil {
		return nil, err
	}
	dst = append(dst, `},"usage":{"output_tokens":`...)
	dst = strconv.AppendInt(dst, int64(outputTokens), 10)
	dst = append(dst, `}}`...)
	return dst, nil
}

func marshalSSEMessageStartBytes(msgID, model string, inputTokens, outputTokens int) ([]byte, error) {
	return appendSSEMessageStart(make([]byte, 0, 192+len(msgID)+len(model)), msgID, model, inputTokens, outputTokens)
}

func marshalSSEContentBlockStartTextBytes(index int) ([]byte, error) {
	return appendSSEContentBlockStartText(make([]byte, 0, 96), index)
}

func marshalSSEContentBlockDeltaTextBytes(index int, text string) ([]byte, error) {
	return appendSSEContentBlockDeltaText(make([]byte, 0, 80+len(text)), index, text)
}

func marshalSSEContentBlockStopBytes(index int) ([]byte, error) {
	return appendSSEContentBlockStop(make([]byte, 0, 48), index)
}

func marshalSSEMessageDeltaBytes(stopReason string, outputTokens int) ([]byte, error) {
	return appendSSEMessageDelta(make([]byte, 0, 88+len(stopReason)), stopReason, outputTokens)
}

// appendAnthropicError renders the Anthropic protocol's in-band failure event.
//
// The wire protocol carries a failure as `event: error` with an
// {"type":"error","error":{...}} payload. It is the only report available once a
// stream has sent its message_start: the HTTP status is already 200 and can never
// be revisited, so a stream that invents an assistant message saying "the accounts
// have exhausted their quota" is indistinguishable from one that answered.
func appendAnthropicError(dst []byte, code, message string) ([]byte, error) {
	dst = append(dst, `{"type":"error","error":{"type":`...)
	var err error
	if dst, err = appendJSONBytes(dst, code); err != nil {
		return nil, err
	}
	dst = append(dst, `,"message":`...)
	if dst, err = appendJSONBytes(dst, message); err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func marshalAnthropicErrorBytes(code, message string) ([]byte, error) {
	return appendAnthropicError(make([]byte, 0, 64+len(code)+len(message)), code, message)
}

// appendOpenAIError renders the failure an OpenAI-compatible stream client looks
// for: a single data frame whose payload is an error object. The terminal [DONE]
// sentinel follows it, so a client that reads to the end of the stream terminates
// instead of waiting for a chunk that will never come.
func appendOpenAIError(dst []byte, code, message string) ([]byte, error) {
	dst = append(dst, `{"error":{"message":`...)
	var err error
	if dst, err = appendJSONBytes(dst, message); err != nil {
		return nil, err
	}
	dst = append(dst, `,"type":`...)
	if dst, err = appendJSONBytes(dst, code); err != nil {
		return nil, err
	}
	dst = append(dst, `,"code":`...)
	if dst, err = appendJSONBytes(dst, code); err != nil {
		return nil, err
	}
	dst = append(dst, `}}`...)
	return dst, nil
}

func marshalOpenAIErrorBytes(code, message string) ([]byte, error) {
	return appendOpenAIError(make([]byte, 0, 96+len(code)+len(message)), code, message)
}
