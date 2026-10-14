package handler

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/audit"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type captureAuditLogger struct {
	events []audit.Event
}

type encodeFailResponseWriter struct {
	header http.Header
	err    error
}

func (w *encodeFailResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}
func (w *encodeFailResponseWriter) Write([]byte) (int, error) { return 0, w.err }
func (w *encodeFailResponseWriter) WriteHeader(int)           {}

func (l *captureAuditLogger) Log(_ context.Context, event audit.Event) {
	l.events = append(l.events, event)
}

type mockUpstreamEdge struct {
	events []upstream.SSEMessage
}

type errorUpstreamEdge struct {
	err   error
	calls int
}

type usageThenErrorUpstreamEdge struct {
	err   error
	calls int
}

func (m *usageThenErrorUpstreamEdge) SendRequestWithPayload(_ context.Context, _ upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), _ *debug.Logger) error {
	m.calls++
	onMessage(upstream.SSEMessage{Type: "model.tokens-used", Event: map[string]any{"inputTokens": 7, "outputTokens": 0}})
	return m.err
}

func (m *mockUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	for _, e := range m.events {
		onMessage(e)
	}
	return nil
}

func (m *errorUpstreamEdge) SendRequestWithPayload(ctx context.Context, req upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), logger *debug.Logger) error {
	m.calls++
	return m.err
}

type partialErrorUpstreamEdge struct {
	err error
}

func (m *partialErrorUpstreamEdge) SendRequestWithPayload(_ context.Context, _ upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), _ *debug.Logger) error {
	onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "partial draft"}})
	return m.err
}

func TestHandleMessages_NonStreamPartialFailureReturnsOnlyError(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = &partialErrorUpstreamEdge{err: errors.New("upstream HTTP 500")}
	payload := map[string]any{"model": "test", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": false}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body)))
	testutil.Equal(t, rec.Code, http.StatusBadGateway)
	testutil.MustNotContainAny(t, rec.Body.String(), "partial draft", "choices")
}

func TestHandleMessages_UsageEvidenceSuppressesReplay(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10, MaxRetries: 2}, nil)
	client := &usageThenErrorUpstreamEdge{err: errors.New("connection reset by peer")}
	h.client = client
	payload := map[string]any{"model": "test", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": false}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body)))

	testutil.Equal(t, client.calls, 1)
	testutil.Equal(t, rec.Code, http.StatusBadGateway)
}

func TestHandleMessages_NonStreamEncodeFailureIsObservable(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model.text-delta", Event: map[string]any{"delta": "answer"}},
		{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}},
	}}
	auditLog := &captureAuditLogger{}
	h.SetAuditLogger(auditLog)
	payload := map[string]any{"model": "test", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": false}
	body, _ := json.Marshal(payload)
	writer := &encodeFailResponseWriter{err: errors.New("client connection closed")}
	h.HandleMessages(writer, httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body)))

	testutil.Equal(t, len(auditLog.events), 1)
	testutil.Equal(t, auditLog.events[0].Status, "error")
}

func TestHandleMessages_StreamPartialFailureEndsWithErrorNotSuccess(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = &partialErrorUpstreamEdge{err: errors.New("upstream HTTP 500")}
	payload := map[string]any{"model": "test", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body)))
	out := rec.Body.String()
	testutil.MustContain(t, out, "event: error")
	testutil.MustNotContain(t, out, "event: message_stop")
}

type finishThenErrorUpstreamEdge struct{ calls int }

func (m *finishThenErrorUpstreamEdge) SendRequestWithPayload(_ context.Context, _ upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), _ *debug.Logger) error {
	m.calls++
	onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}})
	return errors.New("connection reset after finish")
}

func TestHandleMessages_DoesNotRetryAfterTerminalFinish(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10, MaxRetries: 2}, nil)
	client := &finishThenErrorUpstreamEdge{}
	h.client = client
	payload := map[string]any{"model": "test", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body)))
	testutil.Equal(t, client.calls, 1)
	testutil.Equal(t, strings.Count(rec.Body.String(), "event: message_stop"), 1)
}

func TestHandleMessages_Stream_NoFinish_StillStops(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "hello"}},
		// no finish
	}}
	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   true,
	}
	b, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec, req)
	out := rec.Body.String()
	testutil.MustContain(t, out, "hello")
	testutil.MustContain(t, out, "event: message_stop")
}

func TestHandleMessages_WorkBuddyStreamQuotaRetrySkipsRetryMarkerAndCoolsDownFailedAccount(t *testing.T) {
	s := newTestRedisStore(t, "test:")

	first := &store.Account{
		AccountType: "workbuddy",
		Enabled:     true,
		Weight:      1,
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), first), "CreateAccount(first) error = %v")
	second := &store.Account{
		AccountType:   "workbuddy",
		Enabled:       true,
		Weight:        1,
		MaxConcurrent: 2,
	}
	testutil.NoError(t, s.CreateAccount(context.Background(), second), "CreateAccount(second) error = %v")

	publishModel(t, s, &store.Model{Channel: "WorkBuddy", ModelID: "claude-opus-5"})

	lb := loadbalancer.NewWithCacheTTL(s, time.Second)

	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 1, RetryDelay: 0}
	h := NewWithLoadBalancer(cfg, lb)
	h.connTracker = newSpyConnTracker(map[int64]int64{
		first.ID:  0,
		second.ID: 1,
	})
	h.SetClientFactory(func(acc *store.Account, cfg *config.Config) UpstreamClient {
		if acc.ID == first.ID {
			return &errorUpstreamEdge{err: errors.New("workbuddy API error: code=insufficient_funds, status=402, message=Available funding is insufficient for this request.")}
		}
		return &mockUpstreamEdge{events: []upstream.SSEMessage{
			{Type: "model", Event: map[string]any{"type": "text-start"}},
			{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "quota-ok"}},
			{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
		}}
	})

	payload := map[string]any{
		"model":    "claude-opus-5",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   true,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusOK)
	out := rec.Body.String()
	testutil.MustContain(t, out, "quota-ok")
	testutil.MustNotContain(t, out, "Retrying request")

	storedFirst, err := s.GetAccount(context.Background(), first.ID)
	testutil.NoError(t, err, "GetAccount(first) error = %v")
	testutil.Equal(t, storedFirst.StatusCode, store.AccountStatusWorkBuddyQuotaExhausted)
}

func TestHandleMessages_Dedup_DoesNotSuppressInterruptedRetry(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payload := map[string]any{
		"model": "claude-3-5-sonnet",
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "帮我用python写一个计算器"}}},
			{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "(no content)"}}},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "[Request interrupted by user]\n"},
				{"type": "text", "text": "帮我用python写一个计算器"},
			}},
		},
		"system": []any{},
		"stream": false,
	}
	b, _ := json.Marshal(payload)

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec1, req1)
	testutil.Equal(t, rec1.Code, 200)
	testutil.Falsef(t, strings.Contains(rec1.Body.String(), "duplicate_request") || !strings.Contains(rec1.Body.String(), "ok"), "expected first request to complete normally, got: %s", rec1.Body.String())

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(b))
	h.HandleMessages(rec2, req2)
	testutil.Equal(t, rec2.Code, 200)
	testutil.MustNotContain(t, rec2.Body.String(), "duplicate_request")
	testutil.MustContain(t, rec2.Body.String(), "ok")
}

func TestHandleMessages_Dedup_DoesNotSuppressToolResultFollowup(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &mockUpstreamEdge{events: []upstream.SSEMessage{
		{Type: "model", Event: map[string]any{"type": "text-start"}},
		{Type: "model", Event: map[string]any{"type": "text-delta", "delta": "ok"}},
		{Type: "model", Event: map[string]any{"type": "finish", "finishReason": "stop"}},
	}}

	payloadWithToolResult := func(content string) map[string]any {
		return map[string]any{
			"model": "claude-3-5-sonnet",
			"messages": []map[string]any{
				{"role": "user", "content": "帮我优化这个项目"},
				{"role": "assistant", "content": []map[string]any{
					{
						"type":  "tool_use",
						"id":    "tool_1",
						"name":  "Read",
						"input": map[string]any{"file_path": "/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"},
					},
				}},
				{"role": "user", "content": []map[string]any{
					{
						"type":        "tool_result",
						"tool_use_id": "tool_1",
						"content":     content,
					},
				}},
			},
			"system": []any{},
			"stream": false,
		}
	}

	bodyA, _ := json.Marshal(payloadWithToolResult("file one"))
	bodyB, _ := json.Marshal(payloadWithToolResult("file two"))

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(bodyA))
	h.HandleMessages(rec1, req1)
	testutil.Equal(t, rec1.Code, 200)
	testutil.MustContain(t, rec1.Body.String(), "ok")

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(bodyB))
	h.HandleMessages(rec2, req2)
	testutil.Equal(t, rec2.Code, 200)
	testutil.MustNotContain(t, rec2.Body.String(), "duplicate_request")
	testutil.MustContain(t, rec2.Body.String(), "ok")
}

func TestHandleMessages_CanceledFollowup_DoesNotEmitGenericEmptyFallback(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10}
	h := NewWithLoadBalancer(cfg, nil)
	h.client = &errorUpstreamEdge{err: context.Canceled}

	payload := map[string]any{
		"model":           "claude-3-5-sonnet",
		"conversation_id": "test-conversation",
		"messages": []map[string]any{
			{"role": "user", "content": "帮我优化这个项目"},
			{"role": "assistant", "content": []map[string]any{
				{
					"type":  "tool_use",
					"id":    "tool_1",
					"name":  "Read",
					"input": map[string]any{"file_path": "/Users/dailin/Documents/GitHub/truth_social_scraper/api.py"},
				},
			}},
			{"role": "user", "content": []map[string]any{
				{
					"type":        "tool_result",
					"tool_use_id": "tool_1",
					"content":     "1->import os",
				},
			}},
		},
		"system": []any{},
		"stream": true,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	// Nothing had been written when the upstream cancelled, so the failure is
	// answerable with a status rather than a 200 carrying an error frame. The
	// point of the test is unchanged: the cancellation must be reported, and it
	// must not be dressed up as the generic empty-output fallback.
	testutil.NotEqual(t, rec.Code, http.StatusOK)

	out := rec.Body.String()
	testutil.MustNotContain(t, out, "No output was presented to the user")
	testutil.MustContain(t, out, "error")
	testutil.MustNotContain(t, out, "event: error")
}

func TestHandleMessages_NonRetryableClientErrorReturnsExplicitMessage(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 10, MaxRetries: 3, RetryDelay: 0}
	h := NewWithLoadBalancer(cfg, nil)
	upstreamClient := &errorUpstreamEdge{err: errors.New("workbuddy API error: message=Model not found, please try another model")}
	h.client = upstreamClient
	auditLog := &captureAuditLogger{}
	h.SetAuditLogger(auditLog)

	payload := map[string]any{
		"model":    "claude-3-5-sonnet",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"system":   []any{},
		"stream":   false,
	}
	body, _ := json.Marshal(payload)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/workbuddy/v1/messages", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	// A failure with nothing sent yet is a failure. Answering 200 with the error as
	// assistant content is what made a client unable to tell a rejection from an
	// answer.
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.Equal(t, upstreamClient.calls, 1)

	out := rec.Body.String()
	testutil.Falsef(t, !strings.Contains(out, "rejected the request parameters or model") || strings.Contains(out, "workbuddy API error"), "expected redacted upstream error, got: %s", out)
	testutil.MustNotContain(t, out, "No output was presented to the user")
	testutil.MustNotContain(t, out, "retries exhausted")
	var response map[string]any
	err := json.Unmarshal(rec.Body.Bytes(), &response)
	testutil.CheckNoError(t, err)
	_, exists := response["choices"]
	testutil.Falsef(t, exists, "error response must not append a synthetic completion: %s", out)
	testutil.Equal(t, len(auditLog.events), 1)
	testutil.Equal(t, auditLog.events[0].Status, "error")
}
