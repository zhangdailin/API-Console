package warp

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/provider"
)

func assertRawRequestRejected(t *testing.T, raw string) {
	t.Helper()
	err := validateRawRequest(json.RawMessage(raw))
	var app *apperrors.AppError
	if !errors.As(err, &app) || app.HTTPStatus != http.StatusBadRequest || app.Code != "invalid_request_error" {
		t.Fatalf("raw request rejection = %v, want invalid_request_error (400)", err)
	}
}

func TestRawRequestAllowsOnlySupportedWireShapes(t *testing.T) {
	for name, raw := range map[string]string{
		"text":         `{"model":"model-a","messages":[{"role":"user","content":"hello"}]}`,
		"stream_false": `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"stream":false}`,
		"stream_true":  `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"stream":true}`,
		"text_blocks":  `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":" 好"}]}]}`,
		"escaped_keys": `{"mo\u0064el":"model-a","messages":[{"role":"user","content":"hello"}]}`,
		"whitespace":   " \n {\"model\":\"model-a\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"stream\": false } \t ",
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRawRequest(json.RawMessage(raw)); err != nil {
				t.Fatalf("supported wire shape rejected: %v", err)
			}
		})
	}
	registered, ok := provider.Lookup("warp")
	if !ok || registered.ValidateRequest == nil {
		t.Fatal("Warp must register the raw request validator")
	}
	if err := registered.ValidateRequest(json.RawMessage(`{"model":"model-a","messages":[{"role":"user","content":"hello"}],"seed":0}`)); err == nil {
		t.Fatal("registered raw validator accepted unsupported generation controls")
	}
}

func TestRawRequestRejectsEveryAdditionalControlByPresence(t *testing.T) {
	// Zero, false, empty and null values are still explicit controls: shared
	// normalization must not silently discard them before Warp's allowlist.
	fields := []string{
		"n", "seed", "penalties", "presence_penalty", "frequency_penalty", "logprobs", "top_logprobs", "logit_bias",
		"web_search_options", "output_config", "reasoning", "reasoning_effort", "verbosity",
		"max_output_tokens",
		"functions", "function_call", "mcp_servers",
		"response_format", "text", "include", "modalities", "audio", "prediction",
		"client_metadata", "conversation_id", "prompt_cache_key", "instructions",
		"user", "service_tier", "background", "unknown_control", "Model", "Messages", "Stream",
	}
	for _, field := range fields {
		t.Run(field, func(t *testing.T) {
			for _, value := range []string{"null", "false", "0", `""`, "[]", "{}"} {
				t.Run(value, func(t *testing.T) {
					assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"`+field+`":`+value+`}`)
				})
			}
		})
	}
}

func TestRawRequestRejectsUnknownNestedMessageAndBlockFields(t *testing.T) {
	for _, field := range []string{"name", "tool_calls", "tool_call_id", "function_call", "reasoning_content", "refusal", "annotations", "metadata", "unknown", "Role", "Content"} {
		t.Run("message/"+field, func(t *testing.T) {
			for _, value := range []string{"null", "false", "0", `""`, "[]", "{}"} {
				assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":"hello","`+field+`":`+value+`}]}`)
			}
		})
	}
	for name, raw := range map[string]string{
		"later_message":       `{"model":"model-a","messages":[{"role":"user","content":"hello"},{"role":"user","content":"next","unknown":null}]}`,
		"later_block":         `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":"next","unknown":null}]}]}`,
		"escaped_unknown_key": `{"model":"model-a","messages":[{"role":"user","content":"hello"}],"s\u0065ed":0}`,
	} {
		t.Run(name, func(t *testing.T) { assertRawRequestRejected(t, raw) })
	}
	for _, field := range []string{"cache_control", "citations", "annotations", "image_url", "source", "thinking", "signature", "metadata", "unknown", "Type", "Text"} {
		t.Run("block/"+field, func(t *testing.T) {
			for _, value := range []string{"null", "false", "0", `""`, "[]", "{}"} {
				assertRawRequestRejected(t, `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":"hello","`+field+`":`+value+`}]}]}`)
			}
		})
	}
}

func TestRawRequestRejectsMalformedAndWrongShapedFields(t *testing.T) {
	cases := map[string]string{
		"empty": "", "invalid_json": "{", "trailing_json": `{} {}`, "null": "null", "array": "[]", "string": `"request"`, "number": "1", "boolean": "false",
		"missing_model":      `{"messages":[{"role":"user","content":"hello"}]}`,
		"missing_messages":   `{"model":"model-a"}`,
		"null_model":         `{"model":null,"messages":[]}`,
		"numeric_model":      `{"model":1,"messages":[]}`,
		"object_model":       `{"model":{},"messages":[]}`,
		"null_messages":      `{"model":"model-a","messages":null}`,
		"object_messages":    `{"model":"model-a","messages":{}}`,
		"string_messages":    `{"model":"model-a","messages":"hello"}`,
		"null_message":       `{"model":"model-a","messages":[null]}`,
		"string_message":     `{"model":"model-a","messages":["hello"]}`,
		"array_message":      `{"model":"model-a","messages":[[]]}`,
		"missing_role":       `{"model":"model-a","messages":[{"content":"hello"}]}`,
		"null_role":          `{"model":"model-a","messages":[{"role":null,"content":"hello"}]}`,
		"number_role":        `{"model":"model-a","messages":[{"role":1,"content":"hello"}]}`,
		"missing_content":    `{"model":"model-a","messages":[{"role":"user"}]}`,
		"null_content":       `{"model":"model-a","messages":[{"role":"user","content":null}]}`,
		"number_content":     `{"model":"model-a","messages":[{"role":"user","content":1}]}`,
		"bool_content":       `{"model":"model-a","messages":[{"role":"user","content":false}]}`,
		"object_content":     `{"model":"model-a","messages":[{"role":"user","content":{"type":"text","text":"hello"}}]}`,
		"null_block":         `{"model":"model-a","messages":[{"role":"user","content":[null]}]}`,
		"string_block":       `{"model":"model-a","messages":[{"role":"user","content":["hello"]}]}`,
		"missing_block_type": `{"model":"model-a","messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
		"null_block_type":    `{"model":"model-a","messages":[{"role":"user","content":[{"type":null,"text":"hello"}]}]}`,
		"non_text_block":     `{"model":"model-a","messages":[{"role":"user","content":[{"type":"image_url","text":"hello"}]}]}`,
		"missing_block_text": `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		"null_block_text":    `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":null}]}]}`,
		"number_block_text":  `{"model":"model-a","messages":[{"role":"user","content":[{"type":"text","text":1}]}]}`,
		"null_stream":        `{"model":"model-a","messages":[],"stream":null}`,
		"number_stream":      `{"model":"model-a","messages":[],"stream":0}`,
		"string_stream":      `{"model":"model-a","messages":[],"stream":"false"}`,
		"object_stream":      `{"model":"model-a","messages":[],"stream":{}}`,
		"array_stream":       `{"model":"model-a","messages":[],"stream":[]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) { assertRawRequestRejected(t, raw) })
	}
}
