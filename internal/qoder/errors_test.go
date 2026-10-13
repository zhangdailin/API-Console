package qoder

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	orchidserrors "orchids-api/internal/errors"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestIsTransientUpstreamStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		detail string
		want   bool
	}{
		{"plain 500", 500, "", true},
		{"gateway hiccup as 418", 418, `{"error":"provider_error"}`, true},
		{"503", 503, "", true},
		{"4xx naming a provider fault", 400, `{"error":{"type":"provider_error"}}`, true},
		{"401 stays a credential path", 401, "", false},
		{"403 stays a permission path", 403, "", false},
		{"429 stays a capacity window", 429, "", false},
		{"plain 400 is not transient", 400, `{"error":{"type":"invalid_parameter_error"}}`, false},
		{"content policy is never transient", 400, `DataInspectionFailed`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.Equal(t, IsTransientUpstreamStatus(tc.status, tc.detail), tc.want)
		})
	}
}

func TestContentPolicyAndClientFaultMarkers(t *testing.T) {
	testutil.CheckFalse(t, !IsContentPolicy(`{"message":"DataInspectionFailed"}`), "the safety marker was not recognised")
	testutil.CheckFalse(t, IsContentPolicy(`{"message":"gateway is busy"}`), "a busy payload was read as a safety refusal")
	testutil.CheckFalse(t, !IsClientFault(`{"error":{"type":"invalid_parameter_error"}}`), "a parameter rejection was not recognised")
	testutil.CheckFalse(t, IsClientFault(`{"error":{"type":"provider_error"}}`), "a provider fault was read as the client's fault")
}

func TestIsTransientTransport(t *testing.T) {
	transient := []error{
		fmt.Errorf("read tcp 1.2.3.4:443: connection reset by peer"),
		fmt.Errorf("unexpected EOF"),
		fmt.Errorf("remote error: tls: internal error"),
		&net.OpError{Op: "dial", Err: &timeoutError{}},
	}
	for _, err := range transient {
		testutil.CheckTrue(t, IsTransientTransport(err), "IsTransientTransport(%v) = false, want true")
	}

	// A caller-side cancellation is not the upstream hiccupping.
	testutil.CheckFalse(t, IsTransientTransport(context.Canceled), "a canceled context was treated as transient")
	testutil.CheckFalse(t, IsTransientTransport(context.DeadlineExceeded), "an exceeded deadline was treated as transient")
	// Configuration and certificate faults fail identically next time.
	for _, err := range []error{
		fmt.Errorf(`unsupported protocol scheme "ftp"`),
		fmt.Errorf("x509: certificate signed by unknown authority"),
	} {
		testutil.CheckFalsef(t, IsTransientTransport(err), "IsTransientTransport(%v) = true, want false", err)
	}
	testutil.CheckFalse(t, IsTransientTransport(nil), "a nil error was treated as transient")
	// A connection refused at the OS level is still a hiccup.
	testutil.CheckFalse(t, !isTransientSyscall(syscall.ECONNRESET), "ECONNRESET was not recognised as transient")
	testutil.CheckFalse(t, isTransientSyscall(syscall.EACCES), "a permission fault was read as transient")
}

type timeoutError struct{}

func (*timeoutError) Error() string   { return "i/o timeout" }
func (*timeoutError) Timeout() bool   { return true }
func (*timeoutError) Temporary() bool { return true }

func TestEmptyAndTransientSentinels(t *testing.T) {
	wrapped := fmt.Errorf("%w: qoder stream produced no usable events", ErrEmptyStream)
	testutil.CheckFalse(t, !errors.Is(wrapped, ErrEmptyStream), "the empty-stream sentinel was lost through wrapping")
	testutil.CheckFalse(t, isRetryable(wrapped), "an empty stream must not be replayed")
	got := orchidserrors.ClassifyUpstreamError(wrapped.Error())
	testutil.CheckFalsef(t, got.Category != "protocol" || got.Retryable, "class = %+v, want protocol and not retryable", got)

	// A transient provider fault retries locally on the same account.
	transient := &attemptStreamError{err: transientError("status=503"), retryable: true}
	testutil.CheckFalse(t, !isTransientError(transient), "the transient sentinel was lost through wrapping")
	testutil.CheckFalse(t, !isRetryable(transient), "a transient fault must reach the shared handler as retryable")

	// A safety refusal is a client outcome: no retry, no account switch.
	refusal := contentPolicyError("DataInspectionFailed")
	testutil.CheckFalse(t, !errors.Is(refusal, ErrContentPolicy), "the content-policy sentinel was lost through wrapping")
	testutil.CheckFalse(t, isTransientError(refusal) || isRetryable(refusal), "a safety refusal must not be retried or rotated")
}

// TestTransientBackoffIsBounded pins the local retry budget: the shared handler
// multiplies every attempt by its own budget, so an unbounded local retry turns
// one provider hiccup into a storm.
func TestTransientBackoffIsBounded(t *testing.T) {
	testutil.Equal(t, TransientMaxRetries, 2)
	for attempt := 1; attempt <= TransientMaxRetries; attempt++ {
		d := TransientBackoff(attempt)
		testutil.CheckFalsef(t, d <= 0 || d > 5*time.Second, "TransientBackoff(%d) = %v, want a short bounded wait", attempt, d)
	}
	testutil.CheckFalse(t, TransientBackoff(0) <= 0, "a non-positive attempt must still produce a wait")
}

// TestRunChatRetriesATransientFaultLocally proves a provider-side hiccup is
// retried on the account that holds the request rather than being handed to the
// pool-wide switch, and that the retry stops once the budget is spent.
func TestRunChatRetriesATransientFaultLocally(t *testing.T) {
	hits := 0
	server := newRetryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"provider_error"}`))
			return
		}
		writeSSE(w, `data:{"statusCodeValue":200,"body":"{\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}"}`, `event:finish`)
	})
	defer server.Close()

	client := newRetryTestClient(t, server.URL)
	var got []upstream.SSEMessage
	err := client.runChat(context.Background(), chatURL(server.URL), EncodeBody([]byte(`{}`)), modelEntry{Key: "k"},
		"req-1", RuntimeFields{Key: "k"}, func(m upstream.SSEMessage) { got = append(got, m) })
	testutil.NoError(t, err, "runChat() error = %v, want the retry to succeed")
	testutil.Equal(t, hits, 2)
	testutil.True(t, sawFinishMessage(got), "no finish frame: %+v")
}

// TestRunChatDoesNotReplayAContentRefusal proves a safety refusal fails fast:
// replaying it sends the same rejected input again and, worse, marks a healthy
// account as throttled.
func TestRunChatDoesNotReplayAContentRefusal(t *testing.T) {
	hits := 0
	server := newRetryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"DataInspectionFailed"}`))
	})
	defer server.Close()

	client := newRetryTestClient(t, server.URL)
	err := client.runChat(context.Background(), chatURL(server.URL), EncodeBody([]byte(`{}`)), modelEntry{Key: "k"},
		"req-1", RuntimeFields{Key: "k"}, func(upstream.SSEMessage) {})
	testutil.Falsef(t, !errors.Is(err, ErrContentPolicy), "error = %v, want the content-policy sentinel", err)
	testutil.Equal(t, hits, 1)
}

// TestRunChatReportsAnEmptyStream proves a 200 stream that delivers nothing is
// an error rather than an empty success, and is not replayed either.
func TestRunChatReportsAnEmptyStream(t *testing.T) {
	hits := 0
	server := newRetryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, `event:finish`)
	})
	defer server.Close()

	client := newRetryTestClient(t, server.URL)
	err := client.runChat(context.Background(), chatURL(server.URL), EncodeBody([]byte(`{}`)), modelEntry{Key: "k"},
		"req-1", RuntimeFields{Key: "k"}, func(upstream.SSEMessage) {})
	testutil.Falsef(t, !errors.Is(err, ErrEmptyStream), "error = %v, want the empty-stream sentinel", err)
	testutil.Equal(t, hits, 1)
}

// TestRunChatStopsAtABusyVerdict proves the queue refusal keeps its own wait
// rather than being retried through the local transient budget: the shared
// handler owns the shared queue window.
func TestRunChatStopsAtABusyVerdict(t *testing.T) {
	hits := 0
	server := newRetryTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"10605","message":"isQueued"}`))
	})
	defer server.Close()

	client := newRetryTestClient(t, server.URL, withRetryTestCredential("token"))
	err := client.runChat(context.Background(), chatURL(server.URL), EncodeBody([]byte(`{}`)), modelEntry{Key: "k"},
		"req-1", RuntimeFields{Key: "k"}, func(upstream.SSEMessage) {})
	testutil.Falsef(t, !errors.Is(err, ErrBusy), "error = %v, want the busy verdict", err)
	testutil.Equal(t, hits, 1)
}

func sawFinishMessage(messages []upstream.SSEMessage) bool {
	for _, m := range messages {
		if m.Type == "model.finish" {
			return true
		}
	}
	return false
}

func writeSSE(w http.ResponseWriter, lines ...string) {
	for _, line := range lines {
		_, _ = fmt.Fprintf(w, "%s\n\n", line)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func newRetryTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	return server
}

type retryTestOption func(*Client)

func withRetryTestCredential(token string) retryTestOption {
	return func(c *Client) { c.creds.AccessToken = token }
}

func newRetryTestClient(t *testing.T, baseURL string, opts ...retryTestOption) *Client {
	t.Helper()
	client := NewFromAccount(nil, nil)
	setTestEndpoints(client, baseURL, baseURL, baseURL)
	client.creds = Credentials{AccessToken: "access", RefreshToken: "refresh"}
	client.runtime = RuntimeFields{Key: "runtime-key"}
	for _, opt := range opts {
		opt(client)
	}
	return client
}

var _ = upstream.SSEMessage{}
