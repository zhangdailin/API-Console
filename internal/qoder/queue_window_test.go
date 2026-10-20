package qoder

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestNextQueueWindowDelay(t *testing.T) {
	at := func(seconds float64) time.Time {
		return time.Date(2026, 10, 21, 1, 55, 0, 0, time.UTC).Add(time.Duration(seconds * float64(time.Second)))
	}
	for _, tc := range []struct {
		second float64
		want   time.Duration
	}{
		{0, 8 * time.Second},
		{7.5, 500 * time.Millisecond},
		{8, 3 * time.Second},   // open: step
		{16, 3 * time.Second},  // 16+3 still inside
		{17, 51 * time.Second}, // the next step would leave the window
		{30, 38 * time.Second}, // the 30s hint would land at :00, outside
		{59.5, 8500 * time.Millisecond},
	} {
		testutil.Equal(t, nextQueueWindowDelay(at(tc.second)), tc.want)
	}
}

// queueRefusal is the in-stream p-queue refusal production returns.
func queueRefusal(queueType string) string {
	inner := fmt.Sprintf(`{"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"qfmodel\",\"queueCount\":0,\"queueType\":\"%s\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\",\"waitTime\":30}"}`, queueType)
	return "data:" + mustJSON(map[string]any{"body": inner, "statusCodeValue": 403}) + "\n\n"
}

func TestQueueTypeOfProductionRefusal(t *testing.T) {
	err := classifyRefusal(refusal{status: 403, code: busyCode, detail: `{"isQueued":true,"queueType":"p4","retryAfterSeconds":30}`,
		body: `{"code":"10605"}`, described: fmt.Errorf(`qoder upstream error: status=403, {"isQueued":true,"modelKey":"qfmodel","queueCount":0,"queueType":"p4"}`)})
	testutil.Equal(t, queueTypeOf(err.Error()), "p4")
	testutil.False(t, !isWindowedQueueRefusal(err), "a p4 refusal was not recognised")
	testutil.False(t, isWindowedQueueRefusal(fmt.Errorf(`%w: {"queueType":"slow"}`, ErrBusy)), "the slow queue is not windowed")
	testutil.False(t, isWindowedQueueRefusal(fmt.Errorf(`{"queueType":"p4"}`)), "only a busy verdict is windowed")
}

// windowServer refuses with the given queue type until refusals are spent.
func windowServer(t *testing.T, queueType string, refusals int32, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= refusals {
			_, _ = io.WriteString(w, queueRefusal(queueType))
			return
		}
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
}

func setQueueWindowClock(t *testing.T, now func() time.Time) {
	previous := queueWindowNow
	queueWindowNow = now
	t.Cleanup(func() { queueWindowNow = previous })
}

// justBeforeWindow puts the clock 10ms before a window opens, so a retry
// waits 10ms instead of a real minute.
func justBeforeWindow() time.Time {
	return time.Date(2026, 10, 21, 1, 55, 7, 990_000_000, time.UTC)
}

func TestQueueWindowRetryServesARefusedRequest(t *testing.T) {
	setQueueWindowClock(t, justBeforeWindow)
	var calls atomic.Int32
	server := windowServer(t, "p4", 2, &calls)
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil)
	testutil.NoError(t, err)
	testutil.Equal(t, calls.Load(), int32(3))
}

func TestQueueWindowRetryLeavesOtherRefusalsAlone(t *testing.T) {
	setQueueWindowClock(t, justBeforeWindow)
	for name, tc := range map[string]struct {
		queueType string
		cfg       *config.Config
	}{
		"slow queue": {"slow", nil},
		"disabled":   {"p4", &config.Config{QoderQueueWindowRetryDisabled: true}},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := windowServer(t, tc.queueType, 5, &calls)
			defer server.Close()
			client := NewFromAccount(signedTestAccount(), tc.cfg)
			setTestEndpoints(client, server.URL, server.URL, server.URL)
			err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil)
			testutil.Error(t, err)
			testutil.Equal(t, calls.Load(), int32(1))
		})
	}
}

// TestQueueWindowRetryRespectsTheRequestDeadline: a wait that cannot finish
// before the request's deadline is not started; the refusal is returned.
func TestQueueWindowRetryRespectsTheRequestDeadline(t *testing.T) {
	setQueueWindowClock(t, func() time.Time { return time.Date(2026, 10, 21, 1, 55, 30, 0, time.UTC) }) // 38s to the window
	var calls atomic.Int32
	server := windowServer(t, "p4", 5, &calls)
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := client.SendRequestWithPayload(ctx, upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil)
	testutil.Error(t, err)
	testutil.Equal(t, calls.Load(), int32(1))
	testutil.False(t, time.Since(started) > time.Second, "the request slept past its deadline")
}

// TestQueueWindowRetryIsBounded: the window waits stop at queueWindowMaxWait.
func TestQueueWindowRetryIsBounded(t *testing.T) {
	// A clock stuck 1ms before the window makes every wait 1ms, so only the
	// retry cap can end the loop.
	setQueueWindowClock(t, func() time.Time { return time.Date(2026, 10, 21, 1, 55, 7, 999_000_000, time.UTC) })
	var calls atomic.Int32
	server := windowServer(t, "p4", 1000, &calls)
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil)
	testutil.Error(t, err)
	// 130s of 1ms waits would allow 130000 retries; the retry cap stops it.
	testutil.Equal(t, calls.Load(), int32(queueWindowMaxRetries+1))
}
