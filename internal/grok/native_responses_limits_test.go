package grok

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// nativeResponseFixture is a completed Build response with a message and a
// function call; healthy adds an encrypted reasoning item.
func nativeResponseFixture(healthy bool) map[string]any {
	output := []any{}
	if healthy {
		output = append(output, map[string]any{"type": "reasoning", "id": "rs1", "summary": []any{map[string]any{"type": "summary_text", "text": "consider the request"}}, "encrypted_content": "opaque-proof"})
	}
	text := "PUBLIC_ANSWER"
	if !healthy {
		text = strings.Repeat("PRIVATE_ANALYSIS ", 12)
	}
	output = append(output, map[string]any{"id": "msg1", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}, map[string]any{"id": "fc1", "type": "function_call", "call_id": "call1", "name": "lookup", "arguments": `{"value":1}`})
	return map[string]any{"id": "resp-result", "object": "response", "status": "completed", "output": output, "usage": map[string]any{"input_tokens": 10, "output_tokens": 80, "total_tokens": 90, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}
}

func TestNativeReasoningRecoverySpendsOnePhysicalCall(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_encrypted_content"}`))
			return
		}
		item := body["input"].([]any)[0].(map[string]any)
		if item["content"] != "Prior model reasoning summary:\nold plan" {
			t.Error("recovery sent an unlabelled plan")
		}
		response := nativeResponseFixture(false)
		response["output"] = response["output"].([]any)[:1]
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	for range 3 {
		copy := *acc
		copy.ID = 0
		copy.GrokModels = []string{"grok-4.7"}
		copy.GrokModelsSyncedAt = time.Now()
		if err := h.lb.Store.CreateAccount(context.Background(), &copy); err != nil {
			t.Fatal(err)
		}
	}
	cfg := h.configSnapshot().Clone()
	cfg.AccountSwitchCount = 2
	h.SetConfig(cfg)
	sess, err := h.openCLIAccountSession(context.Background(), nil, "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	payload := map[string]any{"reasoning": map[string]any{"effort": "high"}, "input": []any{map[string]any{"type": "reasoning", "encrypted_content": "bad-proof", "summary": []any{map[string]any{"text": "old plan"}}}}}
	rec := httptest.NewRecorder()
	_, _, result := h.serveNativeResponses(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil), sess, false, ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, payload, "/responses", nil)
	if result.Err != nil || calls.Load() != 2 {
		t.Fatalf("recovery did not complete in one extra call: calls=%d result=%+v", calls.Load(), result)
	}
}
