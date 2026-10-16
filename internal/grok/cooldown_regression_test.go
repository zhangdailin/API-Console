package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/store"
)

func TestPassedNativeEncryptedReasoningKeepsAccountAvailable(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, size := range []int{216, 512} {
			t.Run(fmt.Sprintf("stream=%v/bytes=%d", streaming, size), func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					response := map[string]any{"id": "resp-cooldown", "status": "completed", "output": []any{
						map[string]any{"id": "reason-cooldown", "type": "reasoning", "summary": []any{}, "encrypted_content": strings.Repeat("x", size)},
						map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "A complete useful answer containing enough visible text."}}},
					}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 110, "output_tokens_details": map[string]any{"reasoning_tokens": 32}}}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprint(w, parityFrame("response.completed", map[string]any{"response": response}))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(response)
					}
				}))
				defer upstream.Close()
				h, acc := parityBuildHandler(t, upstream)
				acc.ID, acc.TeamID = 0, t.Name()
				acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7"}, time.Now()
				ctx := WithRequestModel(context.Background(), "grok-4.7")
				if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
					t.Fatal(err)
				}
				for attempt := 0; attempt < 2; attempt++ {
					sess, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
					if err != nil {
						t.Fatalf("account unavailable on turn %d: %v", attempt, err)
					}
					rec := httptest.NewRecorder()
					_, _, result := h.serveNativeResponses(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil).WithContext(ctx), sess, false, ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, map[string]any{"model": "grok-4.7", "input": "hello", "stream": streaming, "reasoning": map[string]any{"effort": "high"}}, "/responses", nil)
					sess.Close()
					if rec.Code != 200 || result.Err != nil || result.Withheld {
						t.Fatalf("turn %d rejected: status=%d result=%+v", attempt, rec.Code, result)
					}
					saved, err := h.lb.Store.GetAccount(ctx, acc.ID)
					if err != nil {
						t.Fatal(err)
					}
					if saved.QualityFailures != 0 || !saved.QualityCooldownUntil.IsZero() {
						t.Fatalf("passed turn parked account: %+v", saved.QualityCooldownUntil)
					}
				}
			})
		}
	}
}

func TestFreeQuotaCooldownRequiresExplicitRejection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantHold bool
	}{
		{"transient outage", 503, `{"code":"service_unavailable","error":"Free usage for model grok-4.7 is temporarily unavailable; retry shortly."}`, false},
		{"permission refusal", 403, `{"code":"permission_denied","error":"Free usage for model grok-4.7 is unavailable on this route."}`, false},
		{"generic throttle", 429, `{"code":"rate_limited","error":"Free usage for model grok-4.7 is temporarily busy."}`, false},
		{"message mentions exhaustion code", 429, `{"error":"subscription:free-usage-exhausted; free usage for model grok-4.7"}`, false},
		{"wrong status", 503, freeUsageRejectionFixture, false},
		{"explicit quota", 429, freeUsageRejectionFixture, true},
		{"explicit payment refusal", 402, freeUsageRejectionFixture, true},
		{"nested quota refusal", 403, `{"error":{"code":"subscription:free-usage-exhausted","message":"You've used all the included free usage for model grok-4.7."}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer upstream.Close()
			h, acc := parityBuildHandler(t, upstream)
			acc.ID, acc.TeamID = 0, t.Name()
			acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7", "grok-4.6"}, time.Now()
			ctx := context.Background()
			if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
				t.Fatal(err)
			}
			_, err := h.buildClient().doResponsesAt(ctx, acc, "/responses", map[string]any{"model": "grok-4.7", "input": "hello"})
			if upstreamStatus(err) != tc.status {
				t.Fatalf("unexpected error: %v", err)
			}
			saved, err := h.lb.Store.GetAccount(ctx, acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			hold := store.ModelCooldownRemaining(saved, "grok-4.7", time.Now())
			if tc.wantHold && hold < 23*time.Hour {
				t.Fatalf("explicit refusal not retained: %v", hold)
			}
			if !tc.wantHold && hold > time.Minute {
				t.Fatalf("non-exhaustion error created long cooldown: %v", hold)
			}
			if !saved.GrokFreeQuota.ResetAt.IsZero() || store.ModelCooldownRemaining(saved, "grok-4.6", time.Now()) > 0 {
				t.Fatal("model refusal poisoned unrelated models")
			}
		})
	}
}

func TestNativeCommentaryWithToolCallKeepsAccountAvailable(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			tool := map[string]any{"type": "function_call", "id": "fc-mixed", "call_id": "call-mixed", "name": "lookup", "arguments": `{"value":1}`}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response := map[string]any{"id": "resp-mixed", "status": "completed", "output": []any{
					map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "I will inspect the requested resource before making the change."}}}, tool,
				}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 40, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, parityFrame("response.output_text.delta", map[string]any{"delta": "I will inspect the requested resource before making the change."}))
				_, _ = fmt.Fprint(w, parityFrame("response.output_item.added", map[string]any{"item": tool}))
				_, _ = fmt.Fprint(w, parityFrame("response.output_item.done", map[string]any{"item": tool}))
				_, _ = fmt.Fprint(w, parityFrame("response.completed", map[string]any{"response": response}))
			}))
			defer upstream.Close()
			h, acc := parityBuildHandler(t, upstream)
			acc.ID, acc.TeamID = 0, t.Name()
			acc.GrokModels, acc.GrokModelsSyncedAt = []string{"grok-4.7"}, time.Now()
			ctx := WithRequestModel(context.Background(), "grok-4.7")
			if err := h.lb.Store.CreateAccount(ctx, acc); err != nil {
				t.Fatal(err)
			}
			for turn := 0; turn < 2; turn++ {
				sess, err := h.openCLIAccountSession(ctx, nil, "grok-4.7")
				if err != nil {
					t.Fatal(err)
				}
				rec := httptest.NewRecorder()
				_, _, result := h.serveNativeResponses(rec, httptest.NewRequest("POST", "/grok/v1/responses", nil).WithContext(ctx), sess, false, ModelSpec{ID: "grok-4.7", UpstreamModel: "grok-4.7"}, map[string]any{"model": "grok-4.7", "input": "inspect resource", "stream": stream, "reasoning": map[string]any{"effort": "high"}, "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}, "/responses", nil)
				sess.Close()
				if rec.Code != 200 || result.Err != nil || result.Withheld || result.Quality.ToolCalls != 1 || !strings.Contains(rec.Body.String(), "call-mixed") {
					t.Fatalf("mixed tool turn rejected or duplicated: code=%d result=%+v", rec.Code, result)
				}
				saved, err := h.lb.Store.GetAccount(ctx, acc.ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.QualityFailures != 0 || !saved.QualityCooldownUntil.IsZero() {
					t.Fatalf("tool turn parked account: %+v", saved.QualityCooldownUntil)
				}
			}
		})
	}
}

func TestNativeToolEvidenceDoesNotBypassDumpDetector(t *testing.T) {
	q := newNativeResponseQuality(httptest.NewRecorder(), qualityHoldPolicy{})
	result := chatOutcome{Quality: qualitySignals{ExpectReasoning: true, VisibleChars: 200, EncryptedChars: 512, ReasoningTokens: 100, FirstVisibleMS: 1, LastVisibleMS: 2, VisibleTimingObserved: true, Terminal: true}}
	q.result, q.hold.outcome = &result, &result
	q.observeToolCall(map[string]any{"type": "function_call", "call_id": "dump-call"}, &result)
	if err := q.check(true); err != nil {
		t.Fatal(err)
	}
	if !result.Withheld || result.WithheldReason == qualityReasonNoReasoning {
		t.Fatalf("tool bypassed dump detection: %+v", result)
	}
}
