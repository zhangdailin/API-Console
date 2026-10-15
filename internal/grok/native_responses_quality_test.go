package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/responses"
)

func nativeQualityFixture(healthy bool) map[string]any {
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

func TestNativeResponsesQualityRetriesWithoutLeakingAnalysis(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				response := nativeQualityFixture(n > 1)
				if !streaming {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if n > 1 {
					_, _ = fmt.Fprint(w, parityFrame("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs1", "delta": "consider the request"}))
				}
				text := response["output"].([]any)
				idx := 0
				if n > 1 {
					idx = 1
				}
				_, _ = fmt.Fprint(w, parityFrame("response.output_text.delta", map[string]any{"delta": text[idx].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]}))
				_, _ = fmt.Fprint(w, parityFrame("response.completed", map[string]any{"response": response}))
			}))
			defer server.Close()
			h, acc := parityBuildHandler(t, server)
			for i := 0; i < 2; i++ {
				copy := *acc
				copy.ID = 0
				copy.GrokModels = []string{"grok-4.7"}
				copy.GrokModelsSyncedAt = time.Now()
				copy.OAuthAccessToken = jwtWithClaims(t, fmt.Sprintf(`{"sub":"quality-%d"}`, i))
				if err := h.lb.Store.CreateAccount(context.Background(), &copy); err != nil {
					t.Fatal(err)
				}
			}
			sess, err := h.openCLIAccountSession(context.Background(), nil, "grok-4.7")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			payload := map[string]any{"input": "hello", "model": "grok-4.7", "reasoning": map[string]any{"effort": "high"}, "stream": streaming}
			rec := httptest.NewRecorder()
			id, _, result := h.serveNativeResponses(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil), sess, false, ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, payload, "/responses", nil)
			if result.Err != nil || calls.Load() != 2 || id != "resp-result" {
				t.Fatalf("calls=%d id=%s result=%+v body=%s", calls.Load(), id, result, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, "PRIVATE_ANALYSIS") || !strings.Contains(body, "PUBLIC_ANSWER") || !strings.Contains(body, "call1") {
				t.Fatal(body)
			}
		})
	}
}

func TestNativeResponsesQualityNeverReplaysUnsafeOrPinnedTurns(t *testing.T) {
	for _, mode := range []string{"pinned", "hosted", "budget"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(nativeQualityFixture(false))
			}))
			defer server.Close()
			h, acc := parityBuildHandler(t, server)
			acc.ID = 0
			acc.GrokModels = []string{"grok-4.7"}
			acc.GrokModelsSyncedAt = time.Now()
			for i := 0; i < 2; i++ {
				copy := *acc
				copy.ID = 0
				if err := h.lb.Store.CreateAccount(context.Background(), &copy); err != nil {
					t.Fatal(err)
				}
			}
			cfg := h.configSnapshot().Clone()
			cfg.AccountSwitchCount = 4
			if mode == "budget" {
				cfg.AccountSwitchCount = 1
			}
			h.SetConfig(cfg)
			sess, err := h.openCLIAccountSession(context.Background(), nil, "grok-4.7")
			if err != nil {
				t.Fatal(err)
			}
			defer sess.Close()
			payload := map[string]any{"input": "hello", "reasoning": map[string]any{"effort": "high"}}
			if mode == "hosted" {
				payload["tools"] = []map[string]any{{"type": "web_search"}}
			}
			rec := httptest.NewRecorder()
			_, _, result := h.serveNativeResponses(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil), sess, mode == "pinned", ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, payload, "/responses", nil)
			if calls.Load() != 1 || result.Err == nil || rec.Code != 502 || strings.Contains(rec.Body.String(), "PRIVATE_ANALYSIS") {
				t.Fatalf("calls=%d result=%+v body=%s", calls.Load(), result, rec.Body.String())
			}
		})
	}
}

func TestNativeQualityReleasesReasoningAndKeepsToolOnlyTurns(t *testing.T) {
	rec := httptest.NewRecorder()
	q := newNativeResponseQuality(rec, qualityHoldPolicy{})
	result := chatOutcome{}
	q.observe(map[string]any{"delta": "reasoning"}, "response.reasoning_summary_text.delta", &result)
	if err := q.check(false); err != nil || !q.hold.writer.Committed() {
		t.Fatalf("healthy reasoning was not released: %v", err)
	}
	toolOnly := nativeQualityFixture(false)
	toolOnly["output"] = toolOnly["output"].([]any)[1:]
	raw, _ := json.Marshal(toolOnly)
	rec = httptest.NewRecorder()
	q = newNativeResponseQuality(rec, qualityHoldPolicy{})
	_, _, result = copyNativeResponseWithQuality(q.hold.writer, strings.NewReader(string(raw)), "application/json", "grok-4.7", responses.ToolNamespaces(nil), q)
	if result.Err != nil || result.Withheld || !strings.Contains(rec.Body.String(), "call1") {
		t.Fatalf("tool-only turn was blocked: %+v %s", result, rec.Body.String())
	}
}
