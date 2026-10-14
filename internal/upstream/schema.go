package upstream

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// JSON Schema validation for structured output.
//
// The gateway compiles the caller's schema once and checks the model's answer
// against it. The check exists because forwarding a schema upstream is not the
// same as honoring it: WorkBuddy, Qoder and Cline accept `response_format` and
// then answer in free-form prose, so a client that asked for strict structured
// output silently receives something it cannot parse. Rejecting an answer that
// does not match is the only honest outcome — a wrong one delivered with a 200
// is worse than an error a caller can act on.
//
// The validation is deliberately partial. It covers the subset of draft 2020-12
// that OpenAI's `strict: true` accepts, because that is the subset a strict
// caller can send. Anything outside it is treated as satisfied rather than
// rejected: a gateway that fails a valid answer because its validator is
// incomplete breaks requests that used to work. The rule is check what is
// understood, ignore what is not.
type Schema struct {
	root map[string]any
}

// CompileSchema parses a schema object. It never fails: a schema this gateway
// cannot read is treated as no schema at all, so an unreadable one cannot turn
// a working request into an error.
func CompileSchema(raw any) *Schema {
	object, ok := raw.(map[string]any)
	if !ok || len(object) == 0 {
		return nil
	}
	return &Schema{root: object}
}

// Validate reports whether value satisfies the compiled schema.
func (s *Schema) Validate(value any) error {
	if s == nil || len(s.root) == 0 {
		return nil
	}
	return s.validate(s.root, value, "$", 0)
}

const maxSchemaDepth = 32

// validate walks one schema/value pair. path is a JSON pointer-ish label used
// only in error text, so a caller can find the offending field.
func (s *Schema) validate(schema map[string]any, value any, path string, depth int) error {
	if depth > maxSchemaDepth {
		// A cyclic or pathologically deep schema cannot be walked. Treat it as
		// satisfied: rejecting here would fail every answer, including valid ones.
		return nil
	}
	if refText, ok := schemaString(schema["$ref"]); ok && strings.HasPrefix(refText, "#/$defs/") {
		if target, ok := lookupDef(s.root, strings.TrimPrefix(refText, "#/$defs/")); ok {
			return s.validate(target, value, path, depth+1)
		}
		return nil
	}
	if err := s.validateType(schema, value, path); err != nil {
		return err
	}
	if err := s.validateEnum(schema, value, path); err != nil {
		return err
	}
	if err := s.validateConst(schema, value, path); err != nil {
		return err
	}
	switch typed := value.(type) {
	case map[string]any:
		return s.validateObject(schema, typed, path, depth)
	case []any:
		return s.validateArray(schema, typed, path, depth)
	case string:
		return s.validateString(schema, typed, path)
	case float64, int, int64, json.Number:
		number, _ := valueOfNumberAny(value)
		return s.validateNumber(schema, number, path)
	}
	return nil
}

func (s *Schema) validateType(schema map[string]any, value any, path string) error {
	wanted, ok := schema["type"]
	if !ok {
		return nil
	}
	// "null" as an alternative type is how a strict schema declares an optional
	// field, so a null value is accepted whenever the type list admits it.
	allowed := schemaTypeSet(wanted)
	if len(allowed) == 0 {
		return nil
	}
	if value == nil {
		if allowed["null"] {
			return nil
		}
		return fmt.Errorf("%s: expected %s, got null", path, strings.Join(sortedTypeKeys(allowed), " or "))
	}
	if allowed[jsonTypeOf(value)] {
		return nil
	}
	// JSON has one number type, so "integer" is checked as a number with no
	// fractional part rather than as a distinct type the decoder never produces.
	if allowed["integer"] && jsonTypeOf(value) == "number" {
		if number, ok := valueOfNumberAny(value); ok && !math.IsInf(number, 0) && number == math.Trunc(number) {
			return nil
		}
	}
	return fmt.Errorf("%s: expected %s, got %s", path, strings.Join(sortedTypeKeys(allowed), " or "), jsonTypeOf(value))
}

func (s *Schema) validateObject(schema map[string]any, value map[string]any, path string, depth int) error {
	properties, _ := schema["properties"].(map[string]any)
	required := schemaStringList(schema["required"])
	for _, key := range required {
		if _, ok := value[key]; !ok {
			return fmt.Errorf("%s: missing required property %q", path, key)
		}
	}
	if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
		for key := range value {
			if _, ok := properties[key]; !ok {
				return fmt.Errorf("%s: property %q is not allowed by the schema", path, key)
			}
		}
	}
	for key, item := range value {
		if property, ok := properties[key].(map[string]any); ok {
			if err := s.validate(property, item, path+"."+key, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Schema) validateArray(schema map[string]any, value []any, path string, depth int) error {
	if items, ok := schema["items"].(map[string]any); ok {
		for i, item := range value {
			if err := s.validate(items, item, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Schema) validateString(schema map[string]any, value string, path string) error {
	if format, ok := schemaString(schema["format"]); ok {
		// Only the formats with an unambiguous JSON encoding are checked. An
		// unknown format is a hint the gateway does not enforce.
		switch format {
		case "date-time":
			if !looksLikeDateTime(value) {
				return fmt.Errorf("%s: %q is not a date-time", path, value)
			}
		case "uri":
			if !strings.Contains(value, ":") || strings.ContainsAny(value, " \n\t") {
				return fmt.Errorf("%s: %q is not a uri", path, value)
			}
		}
	}
	return nil
}

func (s *Schema) validateNumber(schema map[string]any, value float64, path string) error {
	if min, ok := valueOfNumberAny(schema["minimum"]); ok && value < min {
		return fmt.Errorf("%s: %v is below the minimum %v", path, value, min)
	}
	if max, ok := valueOfNumberAny(schema["maximum"]); ok && value > max {
		return fmt.Errorf("%s: %v is above the maximum %v", path, value, max)
	}
	return nil
}

func (s *Schema) validateEnum(schema map[string]any, value any, path string) error {
	options, ok := schema["enum"].([]any)
	if !ok || len(options) == 0 {
		return nil
	}
	for _, option := range options {
		if jsonEqual(option, value) {
			return nil
		}
	}
	return fmt.Errorf("%s: value is not one of the allowed enum values", path)
}

func (s *Schema) validateConst(schema map[string]any, value any, path string) error {
	constant, ok := schema["const"]
	if !ok {
		return nil
	}
	if !jsonEqual(constant, value) {
		return fmt.Errorf("%s: value does not match const", path)
	}
	return nil
}

// lookupDef resolves one #/$defs/<name> reference from the schema root.
func lookupDef(root map[string]any, name string) (map[string]any, bool) {
	defs, ok := root["$defs"].(map[string]any)
	if !ok {
		return nil, false
	}
	target, ok := defs[name].(map[string]any)
	return target, ok
}

func schemaString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func schemaStringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, text)
		}
	}
	return out
}

func schemaTypeSet(value any) map[string]bool {
	out := map[string]bool{}
	switch typed := value.(type) {
	case string:
		if text := strings.TrimSpace(typed); text != "" {
			out[text] = true
		}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				out[text] = true
			}
		}
	}
	return out
}

func sortedTypeKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func jsonTypeOf(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64, int, int64, json.Number:
		return "number"
	}
	return "unknown"
}

func valueOfNumberAny(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func jsonEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftRaw, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightRaw, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(leftRaw) == string(rightRaw)
}

// looksLikeDateTime accepts the RFC 3339 shapes a model actually emits,
// including a trailing Z and a fractional second, without pulling in a time
// parse for every string in every answer.
func looksLikeDateTime(value string) bool {
	if len(value) < 19 {
		return false
	}
	digits := func(start, end int) bool {
		for i := start; i < end && i < len(value); i++ {
			if value[i] < '0' || value[i] > '9' {
				return false
			}
		}
		return true
	}
	return digits(0, 4) && value[4] == '-' && digits(5, 7) && value[7] == '-' && digits(8, 10) &&
		(value[10] == 'T' || value[10] == 't') && digits(11, 13) && value[13] == ':' && digits(14, 16) &&
		value[16] == ':' && digits(17, 19)
}
