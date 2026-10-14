package upstream

import (
	"fmt"
	"maps"
	"strings"
)

// ValidateProtocolControls rejects controls with no equivalent in the currently
// supported chat transports. Providers must call this before making a request.
func (r UpstreamRequest) ValidateProtocolControls(provider string) error {
	if len(r.ResponsesTools) > 0 {
		return fmt.Errorf("%s does not support Responses hosted tools", provider)
	}
	if len(r.Include) > 0 {
		return fmt.Errorf("%s does not support Responses include=%v", provider, r.Include)
	}
	for key := range r.ResponseText {
		if key != "format" {
			return fmt.Errorf("%s does not support text.%s", provider, key)
		}
	}
	format := r.ChatResponseFormat()
	if len(format) > 0 {
		switch strings.TrimSpace(fmt.Sprint(format["type"])) {
		case "text", "json_object":
		case "json_schema":
			schema, ok := format["json_schema"].(map[string]interface{})
			if !ok {
				return fmt.Errorf("response_format.json_schema must be an object")
			}
			if _, ok := schema["schema"].(map[string]interface{}); !ok {
				return fmt.Errorf("response_format.json_schema.schema must be an object")
			}
		default:
			return fmt.Errorf("unsupported response format type %q", format["type"])
		}
	}
	// Qoder has no wire field for this optional cache hint. Its request
	// builder omits it; a cache selection hint must not reject generation.
	return nil
}

// StrictSchema returns the compiled schema a strict structured-output request
// asks the model to satisfy, and reports whether the request is strict at all.
//
// Only `strict: true` produces a schema worth checking. A non-strict request is
// a hint to the model, not a contract, so its answer is never rejected here.
// `json_object` names no schema, so it has nothing to validate either.
func (r UpstreamRequest) StrictSchema() (*Schema, bool) {
	format := r.ChatResponseFormat()
	if len(format) == 0 {
		return nil, false
	}
	if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(format["type"])), "json_schema") {
		return nil, false
	}
	envelope, _ := format["json_schema"].(map[string]interface{})
	if len(envelope) == 0 {
		return nil, false
	}
	if strict, ok := envelope["strict"].(bool); !ok || !strict {
		return nil, false
	}
	schema := CompileSchema(envelope["schema"])
	if schema == nil {
		return nil, false
	}
	return schema, true
}

// ChatResponseFormat lowers Responses text.format to the chat spelling without
// changing the client's schema or strictness. It also accepts chat-shaped input.
func (r UpstreamRequest) ChatResponseFormat() map[string]interface{} {
	f := r.ResponseFormat
	if v, ok := r.ResponseText["format"].(map[string]interface{}); ok {
		f = v
	}
	if len(f) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(f))
	maps.Copy(out, f)
	if f["type"] == "json_schema" && f["json_schema"] == nil {
		schema := make(map[string]interface{})
		for _, key := range []string{"name", "schema", "strict", "description"} {
			if v, ok := f[key]; ok {
				schema[key] = v
				delete(out, key)
			}
		}
		out["json_schema"] = schema
		if schema["name"] == nil {
			schema["name"] = "response"
		}
	}
	return out
}
