package upstream

import (
	"encoding/json"
	"fmt"
	"strings"

	"orchids-api/internal/prompt"
)

// StructuredOutput is the validation a strict structured-output request carries
// into the shared passthrough path.
//
// Its only job is to catch the answer that does not match the schema the caller
// declared. Without it the gateway forwards `response_format` upstream, the
// upstream ignores it, and the caller receives free-form prose on a request it
// marked strict — a 200 it cannot parse and cannot distinguish from success.
type StructuredOutput struct {
	schema *Schema
}

// NewStructuredOutput compiles the schema of a strict request. A request that
// is not strict, or whose schema this gateway cannot read, returns nil: nothing
// is enforced and behavior is unchanged.
func NewStructuredOutput(req UpstreamRequest) *StructuredOutput {
	schema, ok := req.StrictSchema()
	if !ok {
		return nil
	}
	return &StructuredOutput{schema: schema}
}

// SystemHint is the instruction prepended to the request's system blocks so the
// model is told the shape before it answers.
//
// The hint is sent in addition to `response_format`, not instead of it: a
// channel that honors the field already constrains generation, and the text
// only restates the contract. The schema itself is appended compacted, because
// a model reads one JSON object far more reliably than a pretty-printed one.
func (s *StructuredOutput) SystemHint() string {
	if s == nil || s.schema == nil {
		return ""
	}
	raw, err := json.Marshal(s.schema.root)
	if err != nil {
		return ""
	}
	return "Respond with JSON only. The response must validate against this JSON Schema. " +
		"Output nothing but the JSON value: no prose, no explanation, and no markdown code fence.\n" +
		"Schema:\n" + string(raw)
}

// Validate checks one assistant answer against the schema.
//
// An answer that is not JSON at all is a rejection: no JSON means no object to
// validate, and a strict caller cannot use it. A fenced block is unwrapped
// first, because a model that wrapped its answer in ```json still produced what
// was asked for. An empty answer is left alone — it is the absence of an answer,
// not a schema violation, and the handler already reports it as such.
func (s *StructuredOutput) Validate(text string) error {
	if s == nil || s.schema == nil {
		return nil
	}
	// An empty answer is not a schema violation; it is the absence of an answer,
	// which the handler already reports as such.
	if strings.TrimSpace(text) == "" {
		return nil
	}
	candidate := extractStructuredCandidate(text)
	if candidate == "" {
		return fmt.Errorf("the model did not return JSON for a strict response_format request")
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(candidate))
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("the model returned text that is not valid JSON: %v", err)
	}
	if err := s.schema.Validate(value); err != nil {
		return fmt.Errorf("the model's answer does not match the requested schema: %v", err)
	}
	return nil
}

// extractStructuredCandidate returns the JSON value inside text, unwrapping a
// markdown code fence and tolerating the surrounding prose a model often adds.
// It returns "" when no object or array is present.
func extractStructuredCandidate(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	if unwrapped, ok := unwrapCodeFence(trimmed); ok {
		trimmed = strings.TrimSpace(unwrapped)
	}
	if trimmed == "" {
		return ""
	}
	for _, open := range []byte{'{', '['} {
		if index := firstUnquotedIndex(trimmed, open); index >= 0 {
			if candidate, ok := balancedJSONPrefix(trimmed[index:], open); ok {
				return candidate
			}
		}
	}
	return ""
}

// unwrapCodeFence strips a leading ``` fence and its trailing close, keeping
// whatever the fence held. An optional language tag is dropped.
func unwrapCodeFence(text string) (string, bool) {
	if !strings.HasPrefix(text, "```") {
		return text, false
	}
	rest := text[3:]
	if newline := strings.IndexByte(rest, '\n'); newline >= 0 {
		rest = rest[newline+1:]
	} else {
		return "", false
	}
	if end := strings.Index(rest, "```"); end >= 0 {
		return rest[:end], true
	}
	return rest, true
}

// firstUnquotedIndex finds want outside a JSON string, so a brace inside a
// string literal is not mistaken for the start of the object.
func firstUnquotedIndex(text string, want byte) int {
	inString, escaped := false, false
	for i := 0; i < len(text); i++ {
		char := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case want:
			return i
		}
	}
	return -1
}

// balancedJSONPrefix returns the shortest prefix that closes the opening
// bracket, scanning strings and escapes so a bracket inside a string does not
// end the scan early. It reports false when the value never balances, which
// means the text is truncated rather than merely decorated.
func balancedJSONPrefix(text string, open byte) (string, bool) {
	close := byte('}')
	if open == '[' {
		close = ']'
	}
	depth, inString, escaped := 0, false, false
	for i := 0; i < len(text); i++ {
		char := text[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case char == '\\':
				escaped = true
			case char == '"':
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return text[:i+1], true
			}
		}
	}
	return "", false
}

// PrependSystemHint puts hint in front of the request's system blocks.
//
// The hint goes first rather than last so a channel that truncates a long
// system array keeps the shape instruction rather than the caller's prose.
func PrependSystemHint(system []prompt.SystemItem, hint string) []prompt.SystemItem {
	if strings.TrimSpace(hint) == "" {
		return system
	}
	out := make([]prompt.SystemItem, 0, len(system)+1)
	out = append(out, prompt.SystemItem{Type: "text", Text: hint})
	return append(out, system...)
}
