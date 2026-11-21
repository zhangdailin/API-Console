package warp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/prompt"
	"orchids-api/internal/upstream"
)

func clientHintsRaw(t *testing.T, fields map[string]any) json.RawMessage {
	t.Helper()
	request := map[string]any{"model": "model-a", "messages": []any{map[string]any{"role": "user", "content": "Synthetic request."}}}
	for key, value := range fields {
		request[key] = value
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertClientHintsError(t *testing.T, raw json.RawMessage, path, reason string) {
	t.Helper()
	var app *apperrors.AppError
	err := validateRawRequest(raw)
	if !errors.As(err, &app) || app.HTTPStatus != 400 || app.Code != "invalid_request_error" {
		t.Fatalf("request rejection = %v, want invalid_request_error (400)", err)
	}
	if !strings.Contains(app.Message, path) || !strings.Contains(app.Message, reason) {
		t.Fatalf("error = %q, want path %q and reason %q", app.Message, path, reason)
	}
}

// These synthetic requests reproduce the four client shapes without retaining
// operational prompts, tool text, credentials or any live-account material.
func TestClientHintsFourSyntheticRequestShapes(t *testing.T) {
	tools := make([]any, 29)
	for i := range tools {
		description := "Synthetic tool description."
		if i == 28 {
			description = strings.Repeat("d", 20<<10)
		}
		tools[i] = map[string]any{"name": fmt.Sprintf("synthetic_%02d", i), "description": description, "input_schema": map[string]any{"type": "object"}}
	}
	for _, test := range []struct {
		name    string
		display string
		ttl     string
		trailer bool
	}{
		{name: "adaptive_cache_context", ttl: "1h"},
		{name: "adaptive_omitted_trailing_system", display: "omitted", ttl: "5m", trailer: true},
		{name: "adaptive_summarized_empty_safeguards", display: "summarized"},
		{name: "adaptive_omitted_cache", display: "omitted", ttl: "1h"},
	} {
		t.Run(test.name, func(t *testing.T) {
			thinking := map[string]any{"type": "adaptive"}
			if test.display != "" {
				thinking["display"] = test.display
			}
			cache := map[string]any{"type": "ephemeral"}
			if test.ttl != "" {
				cache["ttl"] = test.ttl
			}
			fields := map[string]any{
				"thinking":           thinking,
				"context_management": map[string]any{"edits": []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}}},
				"safeguards":         []any{}, "stream": true, "max_tokens": 4096,
				"output_config": map[string]any{"effort": "high"},
				"system":        []any{map[string]any{"type": "text", "text": "Synthetic instructions.", "cache_control": cache}},
				"tools":         tools,
			}
			if test.trailer {
				fields["messages"] = []any{
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Synthetic request.", "cache_control": cache}}},
					map[string]any{"role": "system", "content": "Synthetic trailing instructions."},
				}
			}
			raw := clientHintsRaw(t, fields)
			if err := validateRawRequest(raw); err != nil {
				t.Fatalf("synthetic client wire shape rejected: %v", err)
			}
			var decoded struct {
				Model    string              `json:"model"`
				Messages []prompt.Message    `json:"messages"`
				System   []prompt.SystemItem `json:"system"`
				Tools    []any               `json:"tools"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			prepared, err := validateRequest(upstream.UpstreamRequest{Model: decoded.Model, Messages: decoded.Messages, System: decoded.System, Tools: decoded.Tools})
			if err != nil {
				t.Fatalf("synthetic client normalized shape rejected: %v", err)
			}
			if len(prepared.Tools) != len(tools) || prepared.Tools[28].Description != strings.Repeat("d", 20<<10) {
				t.Fatal("tool declarations were truncated or dropped")
			}
			if !strings.Contains(prepared.Query, "Synthetic instructions.") || !strings.Contains(prepared.Query, "Synthetic request.") || prepared.CurrentText != "Synthetic request." {
				t.Fatal("synthetic request text was not preserved")
			}
			if test.trailer && !strings.Contains(prepared.Query, "Synthetic trailing instructions.") {
				t.Fatal("trailing system instruction was dropped")
			}
		})
	}
}

func TestClientHintsContextManagementOnlyExactNoOps(t *testing.T) {
	for _, edits := range []any{[]any{}, []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}}} {
		if err := validateRawRequest(clientHintsRaw(t, map[string]any{"context_management": map[string]any{"edits": edits}})); err != nil {
			t.Fatalf("context-management no-op rejected: %v", err)
		}
	}
	for name, value := range map[string]any{
		"null": nil, "array": []any{}, "string": "all", "unknown": map[string]any{"unknown": true},
	} {
		t.Run(name, func(t *testing.T) {
			assertClientHintsError(t, clientHintsRaw(t, map[string]any{"context_management": value}), "context_management", "object")
		})
	}
	for name, edits := range map[string]any{
		"missing": nil, "object": map[string]any{}, "null_entry": []any{nil},
		"clear_tools":   []any{map[string]any{"type": "clear_tool_uses_20250919", "keep": "all"}},
		"keep_none":     []any{map[string]any{"type": "clear_thinking_20251015", "keep": "none"}},
		"keep_object":   []any{map[string]any{"type": "clear_thinking_20251015", "keep": map[string]any{"type": "thinking_turns", "value": 1}}},
		"missing_keep":  []any{map[string]any{"type": "clear_thinking_20251015"}},
		"unknown_field": []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all", "trigger": 1}},
		"multiple":      []any{map[string]any{"type": "clear_thinking_20251015", "keep": "all"}, map[string]any{"type": "clear_thinking_20251015", "keep": "all"}},
	} {
		t.Run(name, func(t *testing.T) {
			assertClientHintsError(t, clientHintsRaw(t, map[string]any{"context_management": map[string]any{"edits": edits}}), "context_management.edits", "unsupported")
		})
	}
}

func TestClientHintsSafeguardsCannotBeSilentlyDiscarded(t *testing.T) {
	if err := validateRawRequest(clientHintsRaw(t, map[string]any{"safeguards": []any{}})); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{
		"object": map[string]any{}, "null": nil, "string": "synthetic-sensitive-value", "boolean": false,
		"entry": []any{map[string]any{"type": "synthetic-sensitive-value"}}, "null_entry": []any{nil},
	} {
		t.Run(name, func(t *testing.T) {
			// Safeguards take priority over unknown fields and malformed context.
			raw := clientHintsRaw(t, map[string]any{"safeguards": value, "context_management": nil, "seed": 1})
			for i := 0; i < 20; i++ {
				assertClientHintsError(t, raw, "safeguards", "cannot enforce")
			}
			if err := validateRawRequest(raw); strings.Contains(err.Error(), "synthetic-sensitive-value") {
				t.Fatal("safeguard contents reached a public error")
			}
		})
	}
}

func TestClientHintsThinkingDisplayEnum(t *testing.T) {
	for _, mode := range []string{"enabled", "adaptive", "disabled", "between_tools"} {
		for _, display := range []any{nil, "omitted", "summarized"} {
			if err := validateRawRequest(clientHintsRaw(t, map[string]any{"thinking": map[string]any{"type": mode, "display": display}})); err != nil {
				t.Fatalf("supported thinking display rejected: %v", err)
			}
		}
	}
	for _, value := range []any{"hidden", "visible", "", true, 7, []any{}, map[string]any{}} {
		assertClientHintsError(t, clientHintsRaw(t, map[string]any{"thinking": map[string]any{"type": "adaptive", "display": value}}), "thinking.display", "omitted or summarized")
	}
}

func TestClientHintsCacheControlStrictShapeAndLocations(t *testing.T) {
	for _, role := range []string{"system", "user", "assistant"} {
		for _, ttl := range []string{"", "5m", "1h"} {
			cache := map[string]any{"type": "ephemeral"}
			if ttl != "" {
				cache["ttl"] = ttl
			}
			raw := clientHintsRaw(t, map[string]any{"messages": []any{map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": "Synthetic text.", "cache_control": cache}}}}})
			if err := validateRawRequest(raw); err != nil {
				t.Fatalf("cache marker rejected for role %s TTL %q: %v", role, ttl, err)
			}
		}
	}
	for name, value := range map[string]any{
		"null": nil, "array": []any{}, "string": "ephemeral", "empty": map[string]any{},
		"type": map[string]any{"type": "persistent"}, "type_null": map[string]any{"type": nil},
		"ttl": map[string]any{"type": "ephemeral", "ttl": "24h"}, "ttl_empty": map[string]any{"type": "ephemeral", "ttl": ""},
		"ttl_null": map[string]any{"type": "ephemeral", "ttl": nil}, "ttl_numeric": map[string]any{"type": "ephemeral", "ttl": 300},
		"scope":   map[string]any{"type": "ephemeral", "scope": "global"},
		"unknown": map[string]any{"type": "ephemeral", "synthetic-private-field": true},
	} {
		t.Run(name, func(t *testing.T) {
			raw := clientHintsRaw(t, map[string]any{"system": []any{map[string]any{"type": "text", "text": "Synthetic text.", "cache_control": value}}})
			assertClientHintsError(t, raw, "system[0].cache_control", "Warp request field")
		})
	}
	for _, tc := range []struct {
		name, role, path string
		cache            any
	}{
		{"production_shape", "assistant", "", map[string]any{"type": "ephemeral"}},
		{"invalid_marker", "assistant", ".cache_control.type", map[string]any{"type": "permanent"}},
		{"invalid_ttl", "assistant", ".cache_control.ttl", map[string]any{"type": "ephemeral", "ttl": "24h"}},
		{"null_marker", "assistant", ".cache_control", nil},
		{"wrong_role", "user", "", map[string]any{"type": "ephemeral"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := map[string]any{"type": "tool_use", "id": "call-1", "name": "synthetic", "input": map[string]any{"arg": "value"}, "cache_control": tc.cache}
			raw := clientHintsRaw(t, map[string]any{"messages": []any{
				map[string]any{"role": "system", "content": "Synthetic instructions."},
				map[string]any{"role": "user", "content": "First message."},
				map[string]any{"role": tc.role, "content": []any{map[string]any{"type": "text", "text": "Next."}, block}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "Synthetic result."}}},
			}})
			if tc.name == "production_shape" {
				if err := validateRawRequest(raw); err != nil {
					t.Fatalf("valid historical tool-use cache marker rejected: %v", err)
				}
				return
			}
			assertClientHintsError(t, raw, "messages[2].content[1]"+tc.path, "Warp request field")
		})
	}
}

func TestClientHintsErrorsNeverReflectUnknownKeysOrValues(t *testing.T) {
	private := "synthetic-private-value-\n" + strings.Repeat("x", 256)
	for _, fields := range []map[string]any{
		{private: private},
		{"thinking": map[string]any{"type": "adaptive", private: private}},
		{"metadata": map[string]any{private: private}},
		{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi", private: private}}}}},
	} {
		err := validateRawRequest(clientHintsRaw(t, fields))
		if err == nil || strings.Contains(err.Error(), "synthetic-private-value") || len(err.Error()) > 512 {
			t.Fatalf("unknown client data leaked through error: %v", err)
		}
	}
	assertClientHintsError(t, clientHintsRaw(t, map[string]any{"seed": 0}), "seed", "unsupported request field")
	assertClientHintsError(t, clientHintsRaw(t, map[string]any{"output_config": map[string]any{"format": map[string]any{"type": "json_object"}}}), "output_config.format", "does not support")
}
