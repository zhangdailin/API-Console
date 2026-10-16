package grok

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeRecoveryAndQualitySharePhysicalBudget(t *testing.T) {
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
		response := nativeQualityFixture(false)
		response["output"] = response["output"].([]any)[:1]
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	for i := 0; i < 3; i++ {
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
	if result.Err == nil || calls.Load() != 2 || strings.Contains(rec.Body.String(), "PRIVATE_ANALYSIS") {
		t.Fatalf("budget or hold failed: calls=%d result=%+v", calls.Load(), result)
	}
}

func TestNativeQualityBufferAndDownstreamFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	q := newNativeResponseQuality(rec, qualityHoldPolicy{})
	_, err := q.hold.writer.Write(make([]byte, qualityHoldMaxBytes+1))
	if !errors.Is(err, errQualityBufferLimit) || rec.Body.Len() != 0 || q.hold.writer.Committed() {
		t.Fatal("unverified buffer overflow failed open")
	}
	q = newNativeResponseQuality(compatShortWriter{httptest.NewRecorder()}, qualityHoldPolicy{})
	response := nativeQualityFixture(true)
	raw, _ := json.Marshal(response)
	_, _, result := copyNativeResponseWithQuality(q.hold.writer, strings.NewReader(string(raw)), "application/json", "grok-4.7", nil, q)
	if result.Err == nil {
		t.Fatal("short write was reported as a successful response")
	}
}
