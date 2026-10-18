package qoder

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"orchids-api/internal/prompt"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestClassifyStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		status    int
		body      string
		unauth    bool
		retryable bool
		busy      bool
	}{
		{name: "unauthorized", status: 401, body: `{"message":"login expired"}`, unauth: true},
		{name: "forbidden", status: 403, body: `{"message":"nope"}`, unauth: true},
		{name: "busy under 401", status: 401, body: `{"code":"10605","message":"queue"}`, busy: true, retryable: true},
		{name: "server error", status: 503, body: `{}`, retryable: true},
		{name: "rate limited", status: 429, body: `{}`, retryable: true},
		{name: "bad request", status: 400, body: `{"message":"bad"}`},
	}
	for _, tc := range cases {
		err := classifyStatus(tc.status, "", []byte(tc.body))
		var target *attemptStreamError
		testutil.Falsef(t, !errors.As(err, &target), "%s: error %v is not an attempt error", tc.name, err)
		testutil.CheckEqual(t, target.unauth, tc.unauth)
		testutil.CheckEqual(t, target.retryable, tc.retryable)
		testutil.CheckEqual(t, target.busy, tc.busy)
	}
}

func TestRetryAfterDelayPreservesHintsAndSaturatesSafely(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	testutil.Equal(t, retryAfterDelayAt(now.Add(7*time.Second).Format(http.TimeFormat), now), 7*time.Second)
	testutil.Equal(t, retryAfterDelayAt(now.Add(-time.Second).Format(http.TimeFormat), now), 0)
	testutil.Equal(t, retryAfterDelayAt("120", now), 2*time.Minute)
	testutil.Equal(t, retryAfterDelayAt(now.Add(2*time.Minute).Format(http.TimeFormat), now), 2*time.Minute)
	testutil.Equal(t, retryAfterDelayAt("9223372036854775807", now), time.Duration(1<<63-1))
}

// Long hints reach the handler intact so its finite budget can decline a retry.
// Saturate before multiplication: overflow must never become an early retry.
func TestBusyWaitPreservesHintsAndSaturates(t *testing.T) {
	t.Parallel()

	testutil.Equal(t, busyWait("", []byte(`{"retryAfterMs":600000}`)), 10*time.Minute)
	testutil.Equal(t, busyWait("", []byte(`{"retryAfterSeconds":120}`)), 2*time.Minute)
	testutil.Equal(t, busyWait("", []byte(`{"message":"{\"retryAfterSeconds\":120}"}`)), 2*time.Minute)
	testutil.Equal(t, busyWait("", []byte(`{"retryAfterSeconds":9223372036854775807}`)), time.Duration(1<<63-1))
	testutil.Equal(t, busyWait("", []byte(`{"retryAfterMs":"9223372036854775807"}`)), time.Duration(1<<63-1))
	testutil.Equal(t, busyWait("", []byte(`{"message":"{\"retryAfterSeconds\":29,\"serviceAvailable\":false}"}`)), 29*time.Second)
	testutil.Equal(t, busyWait("", []byte(`{"queue":{"isQueued":true,"waitTime":1500}}`)), 1500*time.Millisecond)
	testutil.Equal(t, busyWait("7", nil), 7*time.Second)
	testutil.Equal(t, busyWait("not-a-number", nil), 2*time.Second)
}

// TestSendRequestRejectsUnsupportedName proves the live request path rejects a
// bad model name before any upstream call.
func TestSendRequestRejectsUnsupportedName(t *testing.T) {
	t.Parallel()

	acc := signedTestAccount()
	client := NewFromAccount(acc, nil)
	setTestEndpoints(client, "http://127.0.0.1:1", "http://127.0.0.1:1", "http://127.0.0.1:1")
	if err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{
		Model:    "definitely-not-a-model",
		Messages: []prompt.Message{{Role: "user", Content: prompt.MessageContent{Text: "ping"}}},
	}, nil, nil); err == nil {
		t.Fatal("SendRequestWithPayload() error = nil for an unsupported model")
	}
}
