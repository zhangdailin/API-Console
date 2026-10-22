package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// sharedRefusalUpstream always fails the way a queue-throttled upstream does, and
// counts the attempts so a test can tell a single probe from a spent budget.
type sharedRefusalUpstream struct {
	calls int
	wait  time.Duration
}

func (m *sharedRefusalUpstream) SendRequestWithPayload(_ context.Context, req upstream.UpstreamRequest, _ func(upstream.SSEMessage), _ *debug.Logger) error {
	m.calls++
	if m.wait > 0 {
		return sharedRefusalHint{wait: m.wait}
	}
	return errors.New(`qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"serviceAvailable\":false,\"retryAfterSeconds\":30}"}`)
}

type sharedRefusalHint struct{ wait time.Duration }

func (e sharedRefusalHint) Error() string {
	return `qoder gateway is busy: {"code":"10605","isQueued":true,"serviceAvailable":false}`
}

func (e sharedRefusalHint) RetryAfter() time.Duration { return e.wait }

// recoveringRefusalUpstream succeeds after its queue probe so the retry path
// must return an answer without requiring another client-side request.
type recoveringRefusalUpstream struct {
	calls int
}

func (m *recoveringRefusalUpstream) SendRequestWithPayload(_ context.Context, _ upstream.UpstreamRequest, onMessage func(upstream.SSEMessage), _ *debug.Logger) error {
	m.calls++
	if m.calls <= 2 {
		return errors.New(`qoder upstream rejected the credential: {"code":"10605","message":"{\"isQueued\":true,\"serviceAvailable\":false,\"retryAfterSeconds\":30}"}`)
	}
	onMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "ready"}})
	onMessage(upstream.SSEMessage{Type: "model.finish", Event: map[string]any{"finishReason": "end_turn"}})
	return nil
}

func TestSharedRefusalRecoversInsideSingleRequest(t *testing.T) {
	cfg := &config.Config{RequestTimeout: 30, MaxRetries: 3, RetryDelay: 1}
	h := NewWithLoadBalancer(cfg, nil)
	stub := &recoveringRefusalUpstream{}
	h.client = stub
	payload := map[string]any{"model": "qwen3.8-flash", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": true}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", bytes.NewReader(body))
	h.HandleMessages(rec, req)
	testutil.Falsef(t, stub.calls != 3 || rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ready"), "calls=%d status=%d body=%s", stub.calls, rec.Code, rec.Body.String())
}

// TestStreamOpensOnlyWhenThereIsSomethingToSend pins the deferral: a streaming
// response must not commit its status before the upstream has produced anything,
// or a failure can no longer be answered with a status.
func TestStreamOpensOnlyWhenThereIsSomethingToSend(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"

	testutil.Equal(t, rec.buf.String(), "")
	testutil.False(t, sh.hasCommitted(), "hasCommitted() = true before any output")

	sh.handleMessage(upstream.SSEMessage{
		Type:  "model.text-delta",
		Event: map[string]any{"delta": "hello"},
	})

	out := rec.buf.String()
	startAt := strings.Index(out, "event: message_start")
	textAt := strings.Index(out, `"text":"hello"`)
	testutil.Falsef(t, startAt < 0 || textAt < 0, "expected an opening frame followed by the text, got: %s", out)
	testutil.Falsef(t, startAt > textAt, "content arrived before the opening frame: %s", out)
	testutil.Equal(t, strings.Count(out, "event: message_start"), 1)
	testutil.False(t, !sh.hasCommitted(), "hasCommitted() = false after the opening frame was written")
}

// TestKeepAliveDoesNotCommitSilentStream keeps the HTTP status available for
// a queue refusal even after the watchdog's first 15-second tick.
func TestKeepAliveDoesNotCommitSilentStream(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"

	sh.writeKeepAlive()
	testutil.Falsef(t, rec.buf.Len() != 0 || sh.hasCommitted(), "keep-alive prematurely opened response: %q", rec.buf.String())
	sh.reportRequestFailure("queue refused", "rate_limit", "Qoder model queue unavailable", 0)
	testutil.Falsef(t, !strings.Contains(rec.buf.String(), "Qoder model queue unavailable") || strings.Contains(rec.buf.String(), "event: message_start"), "queue failure was not returned as HTTP error: %q", rec.buf.String())
}

// TestKeepAliveOpensAStreamSilentPastTheProxyWindow covers a first event that
// takes longer than a fronting proxy waits for an origin. Without opening the
// stream, Cloudflare ended such requests with its own 504 after about 100
// seconds of silence, while the gateway was still waiting on the upstream.
func TestKeepAliveOpensAStreamSilentPastTheProxyWindow(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-max"
	sh.startTime = time.Now().Add(-silentCommitAfter)

	sh.writeKeepAlive()
	out := rec.buf.String()
	testutil.False(t, !sh.hasCommitted(), "a stream silent past silentCommitAfter stayed uncommitted")
	testutil.Equal(t, strings.Count(out, "event: message_start"), 1)
	testutil.Falsef(t, strings.Index(out, ssePing) < strings.Index(out, "event: message_start"), "keep-alive preceded the opening frame: %q", out)

	// A refusal after the stream opened is reported in band and closes it.
	sh.reportRequestFailure("queue refused", "rate_limit", "Qoder model queue unavailable", 0)
	testutil.MustContain(t, rec.buf.String(), "event: error")
	testutil.MustContain(t, rec.buf.String(), "Qoder model queue unavailable")
}

// TestKeepAliveContinuesAfterStreamOpens covers the usual keep-alive behavior
// after an actual content block has committed the response.
func TestKeepAliveContinuesAfterStreamOpens(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"
	sh.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "hello"}})
	sh.writeKeepAlive()
	testutil.MustContain(t, rec.buf.String(), ssePing)
}

// TestKeepAliveFormatFollowsTheProtocol: Claude Code abandoned a Qoder stream
// after ~119 seconds that carried only ": keep-alive" comments ("1 stream
// event received ... none in the final 119282 ms"). An Anthropic stream gets
// the ping event; an OpenAI stream keeps the comment its clients expect.
func TestKeepAliveFormatFollowsTheProtocol(t *testing.T) {
	for format, want := range map[adapter.ResponseFormat]string{
		adapter.FormatAnthropic: ssePing,
		adapter.FormatOpenAI:    sseKeepAlive,
	} {
		rec := newFlushRecorder()
		sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, format)
		sh.pendingModel = "qwen3.8-flash"
		sh.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "hello"}})
		before := rec.buf.Len()
		sh.writeKeepAlive()
		testutil.Equal(t, rec.buf.String()[before:], want)
		sh.release()
	}
	// The ping is a well-formed Anthropic event.
	var ping map[string]any
	testutil.NoError(t, json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(ssePing, "event: ping\ndata: "), "\n\n")), &ping))
	testutil.Equal(t, ping["type"], any("ping"))
}

// TestTerminalOnlyResponseStillOpensTheStream covers an answer that produced no
// content: the opening frame must still precede the terminal events, or the
// client receives a stop for a message that never started.
func TestTerminalOnlyResponseStillOpensTheStream(t *testing.T) {
	rec := newFlushRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatAnthropic)
	defer sh.release()
	sh.pendingModel = "qwen3.8-flash"

	sh.finishResponse("end_turn")

	out := rec.buf.String()
	startAt := strings.Index(out, "event: message_start")
	deltaAt := strings.Index(out, "event: message_delta")
	stopAt := strings.Index(out, "event: message_stop")
	testutil.Falsef(t, startAt < 0 || deltaAt < 0 || stopAt < 0, "expected opening and terminal frames, got: %s", out)
	testutil.Falsef(t, !(startAt < deltaAt && deltaAt < stopAt), "frames out of order: %s", out)
}

// TestSharedRefusalBeforeOutputUsesRetryWindow confirms that the server keeps
// the same request open through its bounded retry window. The upstream queued
// the request and named a window, so the answer is 429 — the retryable capacity
// status an OpenAI-compatible client already backs off from — with the caller's
// retry hint, and never a 503 claiming the upstream service is down while the
// same upstream answers most requests. Nothing is opened as an SSE stream.
func TestSharedRefusalBeforeOutputUsesRetryWindow(t *testing.T) {
	cfg := &config.Config{DebugEnabled: false, RequestTimeout: 30, MaxRetries: 3, RetryDelay: 1}
	h := NewWithLoadBalancer(cfg, nil)
	stub := &sharedRefusalUpstream{}
	h.client = stub

	payload := map[string]any{
		"model":    "qwen3.8-flash",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   true,
	}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", bytes.NewReader(body))

	h.HandleMessages(rec, req)

	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	out := rec.Body.String()
	testutil.MustNotContainAny(t, out, "event: error", "data:")
	testutil.MustContain(t, out, "upstream_queue")
	// Initial request plus the three configured retry probes.
	testutil.Equal(t, stub.calls, 4)
}

// TestSharedRefusalWaitBudgetIsBounded pins the bound on the shared-refusal
// retry wait against the deadline that actually ends the request: the origin
// timeout of the edge proxy in front of this process, which is Cloudflare's
// 100s. Not the caller's patience.
//
// The first version of this bound reasoned from the caller instead, at 90s, and
// production answered 520 — "the origin web server sent a response Cloudflare
// could not parse" — for requests the gateway would have completed at ~106s.
// Caddy's access log showed the shape exactly: every qoder failure before that
// build ended at 33-38s, and every one after it ended at 97-108s, with one
// connection closed by the edge before any response was written (status 0).
//
// The wall-clock model below is measured on that deployment, not assumed: one
// 30s window plus its attempt cost ~33s, and one attempt ~1.6s. So N windows
// cost about 33*N + 1.6*(N+1). The default must admit the most windows that
// still finish inside the edge.
func TestSharedRefusalWaitBudgetIsBounded(t *testing.T) {
	testutil.Equal(t, sharedRefusalTotalWaitBudget, 60*time.Second)

	for _, tc := range []struct {
		name    string
		already time.Duration
		next    time.Duration
		want    bool
	}{
		{"first window fits", 0, 30 * time.Second, true},
		{"second window fits exactly", 30 * time.Second, 30 * time.Second, true},
		{"third window does not: it would overrun the edge", 60 * time.Second, 30 * time.Second, false},
		{"a window that would overrun is refused", 40 * time.Second, 30 * time.Second, false},
		{"nothing to wait for is not a wait", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Equal(t, sharedRefusalWaitAllowedWithin(tc.already, tc.next, sharedRefusalTotalWaitBudget), tc.want)
		})
	}

	// The bound has to exclude the window whose wall clock the edge would cut
	// off, and this is the assertion that would have caught the 520.
	const (
		edgeOriginTimeout = 100 * time.Second
		windowCost        = 33 * time.Second // 30s hint + jitter + one attempt
		attemptCost       = 1600 * time.Millisecond
	)
	wallClock := func(windows int) time.Duration {
		return time.Duration(windows)*windowCost + time.Duration(windows+1)*attemptCost
	}
	windows := int(sharedRefusalTotalWaitBudget / (30 * time.Second))
	testutil.Equal(t, windows, 2)
	got := wallClock(windows)
	testutil.Falsef(t, got >= edgeOriginTimeout, "worst case wall clock = %v, which the %v edge origin timeout cuts off", got, edgeOriginTimeout)
	// And the window it refuses is exactly the one that would have overrun.
	got = wallClock(windows + 1)
	testutil.Falsef(t, got < edgeOriginTimeout, "refusing the third window gives up %v of headroom; it should be the edge that forces the bound", edgeOriginTimeout-got)

}

func TestSharedRefusalConfiguredBudgetStopsBeforeRetry(t *testing.T) {
	for _, budget := range []int{-1, 100} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			cfg := &config.Config{RequestTimeout: 30, MaxRetries: 3, RetryDelay: 1, SharedRefusalWaitBudgetMs: budget}
			h := NewWithLoadBalancer(cfg, nil)
			defer h.Close()
			stub := &sharedRefusalUpstream{wait: 30 * time.Second}
			h.client = stub
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "http://x/qoder/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
			rec := httptest.NewRecorder()
			h.HandleMessages(rec, req)
			if ctx.Err() != nil || stub.calls != 1 || rec.Code != http.StatusTooManyRequests {
				t.Fatalf("calls=%d status=%d context=%v body=%s", stub.calls, rec.Code, ctx.Err(), rec.Body.String())
			}
		})
	}
}
