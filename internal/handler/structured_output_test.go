package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// strictSchemaBody is the shape an OpenAI structured-output client sends: a
// named schema with `strict: true`.
func strictSchemaBody(strict bool) []byte {
	payload := map[string]any{
		"model":    "claude-opus-4-5",
		"messages": []map[string]any{{"role": "user", "content": "give me the answer"}},
		"stream":   false,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "answer",
				"strict": strict,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
						"age":  map[string]any{"type": "integer"},
					},
					"required":             []any{"name", "age"},
					"additionalProperties": false,
				},
			},
		},
	}
	body, _ := json.Marshal(payload)
	return body
}

func strictUpstream(answer string) *mockUpstream {
	return &mockUpstream{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": answer}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}
}

func TestHandleMessages_StrictSchemaRejectsNonConformingAnswer(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	// The upstream ignores response_format and answers in prose, which is exactly
	// what WorkBuddy, WorkBuddy and Cline do today.
	h.client = strictUpstream("Sure! Here is what you asked for.")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(strictSchemaBody(true)))
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.MustNotContain(t, rec.Body.String(), "Here is what you asked for")
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "decode error body: %v")
	testutil.Equal(t, body.Error.Type, "schema_mismatch")
}

func TestHandleMessages_StrictSchemaAcceptsConformingAnswer(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = strictUpstream(`{"name":"ada","age":36}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(strictSchemaBody(true)))
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), "ada")
}

// A non-strict request is a hint to the model, not a contract, so an answer
// that ignores it is still delivered.
func TestHandleMessages_NonStrictSchemaDeliversAnswer(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = strictUpstream("free-form prose")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(strictSchemaBody(false)))
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	testutil.MustContain(t, rec.Body.String(), "free-form prose")
}

// The schema instruction reaches the upstream as a leading system block, so a
// channel that does honor `response_format` is still told the shape.
func TestHandleMessages_StrictSchemaPrependsSystemHint(t *testing.T) {
	up := strictUpstream(`{"name":"ada","age":36}`)
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = up

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-5",
		"messages": []map[string]any{{"role": "user", "content": "give me the answer"}},
		"stream":   false,
		"system":   []any{map[string]any{"type": "text", "text": "be terse"}},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "answer",
				"strict": true,
				"schema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string"},
						"age":  map[string]any{"type": "integer"},
					},
					"required": []any{"name", "age"},
				},
			},
		},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	testutil.False(t, len(up.capturedReqs) == 0, "the upstream was never called")
	system := up.capturedReqs[len(up.capturedReqs)-1].System
	testutil.Falsef(t, len(system) == 0, "no system blocks reached the upstream")
	testutil.MustContain(t, system[0].Text, "Respond with JSON only.")
	testutil.MustContain(t, system[0].Text, `"age"`)
	testutil.Falsef(t, len(system) < 2 || system[1].Text != "be terse", "the caller's system block was lost: %#v", system)
}

// A request with no schema at all must be byte-for-byte the request it was
// before: no hint is prepended and no check is installed.
func TestHandleMessages_NoSchemaLeavesSystemUntouched(t *testing.T) {
	up := strictUpstream("plain answer")
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = up

	body, _ := json.Marshal(map[string]any{
		"model":    "claude-opus-4-5",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   false,
		"system":   []any{map[string]any{"type": "text", "text": "be terse"}},
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)

	system := up.capturedReqs[len(up.capturedReqs)-1].System
	testutil.Equal(t, len(system), 1)
	testutil.Equal(t, system[0].Text, "be terse")
}
