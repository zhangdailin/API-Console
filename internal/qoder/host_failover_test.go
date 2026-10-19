package qoder

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// TestChatFailsOverOnAProviderFaultAndRemembersTheNode covers the primary
// answering with its own fault. Failover used to require that no response
// arrived at all, so a primary answering 5xx was retried in place and
// api1/api3 were never used; and each new request started at the failing
// primary again.
func TestChatFailsOverOnAProviderFaultAndRemembersTheNode(t *testing.T) {
	var primaryCalls, fallbackCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"provider_error"}`)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer fallback.Close()

	newClient := func() *Client {
		client := NewFromAccount(signedTestAccount(), nil)
		setTestEndpoints(client, fallback.URL, fallback.URL, primary.URL)
		client.endpoints.inferenceFallbacks = []string{fallback.URL}
		return client
	}
	request := upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}

	testutil.NoError(t, newClient().SendRequestWithPayload(context.Background(), request, nil, nil))
	testutil.Equal(t, primaryCalls.Load(), int32(1))
	testutil.Equal(t, fallbackCalls.Load(), int32(1))

	// The next request, on any client for the same primary, starts at the node
	// that worked instead of paying for the primary's fault again.
	testutil.NoError(t, newClient().SendRequestWithPayload(context.Background(), request, nil, nil))
	testutil.Equal(t, primaryCalls.Load(), int32(1))
	testutil.Equal(t, fallbackCalls.Load(), int32(2))

	// Once the preference lapses the primary is tried first again.
	hosts := newClient().endpoints.orderedInferenceHosts(time.Now().Add(hostPreferenceTTL))
	testutil.Equal(t, hosts[0], primary.URL)
}

func TestOrderedInferenceHostsKeepsEveryHost(t *testing.T) {
	e := endpoints{inference: "https://primary.test", inferenceFallbacks: []string{"https://one.test", "https://two.test"}}
	now := time.Now()
	testutil.Equal(t, strings.Join(e.orderedInferenceHosts(now), " "), strings.Join([]string{"https://primary.test", "https://one.test", "https://two.test"}, " "))

	preferInferenceHost(e.inference, "https://two.test", now)
	testutil.Equal(t, strings.Join(e.orderedInferenceHosts(now), " "), strings.Join([]string{"https://two.test", "https://primary.test", "https://one.test"}, " "))

	// A host without failover nodes is never reordered.
	custom := endpoints{inference: "https://custom.test"}
	testutil.Equal(t, strings.Join(custom.orderedInferenceHosts(now), " "), "https://custom.test")
}
