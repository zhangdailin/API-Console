package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"
	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// relayRecordingClient replays a fixed answer and records what reached upstream.
// answer is the text the fake model produces; the default is prose, which a
// strict structured-output request must reject.
type relayRecordingClient struct {
	requests []upstream.UpstreamRequest
	answer   string
}

func (c *relayRecordingClient) SendRequestWithPayload(_ context.Context, req upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.requests = append(c.requests, req)
	answer := c.answer
	if answer == "" {
		answer = "upstream answer"
	}
	for _, event := range []map[string]any{{"type": "text-start"}, {"type": "text-delta", "delta": answer}, {"type": "finish", "finishReason": "stop"}} {
		emit(upstream.SSEMessage{Type: "model", Event: event})
	}
	return nil
}

func TestRelayRepeatedRequestsAlwaysReachUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
		client := &relayRecordingClient{}
		h.client = client
		body, _ := json.Marshal(map[string]any{"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "Explain this example"}}, "stream": stream})
		for _, key := range []string{"", "", "once", "once"} {
			r := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
			r.Header.Set("Idempotency-Key", key)
			r.Header.Set("X-Stainless-Retry-Count", "1")
			w := httptest.NewRecorder()
			before := len(client.requests)
			h.HandleMessages(w, r)
			testutil.Falsef(t, w.Code != http.StatusOK || len(client.requests) != before+1 || !strings.Contains(w.Body.String(), "upstream answer"), "valid repeated request suppressed: status=%d calls=%d body=%s", w.Code, len(client.requests), w.Body.String())
		}
	}
}

// TestRelayForwardsCallerHistoryVerbatim guards the passthrough contract for the
// OpenAI-shaped channels. It used to be framed as "a workdir change preserves
// history"; the workdir concept is gone, but the invariant it protected — the
// relay never trims or rewrites what the caller sent — still has to hold.
func TestRelayForwardsCallerHistoryVerbatim(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	client := &relayRecordingClient{}
	h.client = client
	messages := []prompt.Message{
		{Role: "user", Content: prompt.MessageContent{Text: strings.Repeat("Earlier context must survive. ", 100)}},
		{Role: "assistant", Content: prompt.MessageContent{Text: "Earlier response"}},
		{Role: "user", Content: prompt.MessageContent{Text: "Continue explaining the example"}},
	}
	body, _ := json.Marshal(ClaudeRequest{Model: "claude-3-5-sonnet", ConversationID: "relay-history", Messages: messages})
	r := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.HandleMessages(w, r)
	testutil.Falsef(t, w.Code != 200 || len(client.requests) != 1, "status=%d body=%s calls=%d", w.Code, w.Body.String(), len(client.requests))
	got := client.requests[0].Messages
	testutil.Equal(t, len(got), len(messages))
	for i := range messages {
		testutil.Equal(t, got[i].Role, messages[i].Role)
		testutil.Equal(t, got[i].ExtractText(), messages[i].ExtractText())
	}
}

func TestRelayIdempotencyKeyIsCallerScoped(t *testing.T) {
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	client := &relayRecordingClient{}
	h.client = client
	for _, caller := range []string{"caller-a", "caller-b"} {
		r := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"Explain this example"}]}`))
		r.Header.Set("X-API-Key", caller)
		r.Header.Set("Idempotency-Key", "same-key")
		w := httptest.NewRecorder()
		h.HandleMessages(w, r)
		testutil.Equal(t, w.Code, 200)
	}
	testutil.Equal(t, len(client.requests), 2)
}

func TestRelayRepeatedTextIsNotFiltered(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		rec := httptest.NewRecorder()
		h := newStreamHandler(&config.Config{}, rec, debug.New(false, false), streaming, adapter.FormatAnthropic)
		text := "Hi! How can I help you today?"
		h.handleMessage(upstream.SSEMessage{Type: "model.text-start", Event: map[string]any{}})
		for range 3 {
			h.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": text + text}})
		}
		h.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}})
		got := h.responseText.String()
		if streaming {
			got = rec.Body.String()
		}
		testutil.CheckEqual(t, strings.Count(got, text), 6)
		h.release()
	}
}

func TestRelayRepeatedToolCallsHaveIndependentIDs(t *testing.T) {
	for _, format := range []adapter.ResponseFormat{adapter.FormatAnthropic, adapter.FormatOpenAI} {
		for _, streaming := range []bool{false, true} {
			rec := httptest.NewRecorder()
			h := newStreamHandler(&config.Config{}, rec, debug.New(false, false), streaming, format)
			for _, id := range []string{"call_one", "call_two"} {
				h.handleMessage(upstream.SSEMessage{Type: "model.tool-call", Event: map[string]any{"toolCallId": id, "toolName": "Write", "input": `{"file_path":"repeat.txt","content":"same content"}`}})
			}
			h.handleMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "tool_use"}})
			testutil.CheckEqual(t, h.toolCallCount, 2)
			wire := rec.Body.String()
			if !streaming {
				raw, _ := json.Marshal(h.contentBlocks)
				wire = string(raw)
			}
			for _, id := range []string{"call_one", "call_two"} {
				testutil.CheckContain(t, wire, id)
			}
			h.release()
		}
	}
}

type relayConcurrentClient struct {
	entered chan struct{}
	release chan struct{}
}

func (c *relayConcurrentClient) SendRequestWithPayload(ctx context.Context, _ upstream.UpstreamRequest, emit func(upstream.SSEMessage), _ *debug.Logger) error {
	c.entered <- struct{}{}
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	emit(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "parallel answer"}})
	emit(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "stop"}})
	return nil
}

func TestRelayIdenticalInflightRequestsBothReachUpstream(t *testing.T) {
	client := &relayConcurrentClient{entered: make(chan struct{}, 2), release: make(chan struct{})}
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10}, nil)
	h.client = client
	finished := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() {
			r := httptest.NewRequest(http.MethodPost, "/workbuddy/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"Explain the same example"}]}`))
			r.Header.Set("Idempotency-Key", "same-inflight-key")
			w := httptest.NewRecorder()
			h.HandleMessages(w, r)
			finished <- w
		}()
	}
	entered := 0
	for entered < 2 {
		select {
		case <-client.entered:
			entered++
		case <-time.After(3 * time.Second):
			close(client.release)
			t.Fatal("an identical inflight request was suppressed")
		}
	}
	close(client.release)
	for range 2 {
		select {
		case w := <-finished:
			testutil.CheckFalsef(t, w.Code != 200 || !strings.Contains(w.Body.String(), "parallel answer"), "status=%d body=%s", w.Code, w.Body.String())
		case <-time.After(3 * time.Second):
			t.Fatal("parallel request failed to finish")
		}
	}
}
