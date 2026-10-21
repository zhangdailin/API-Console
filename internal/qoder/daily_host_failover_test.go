package qoder

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// dailySpent answers like api2 did for an account whose daily count was spent
// there; the same account was still served on api1.
func dailySpentServer(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"Billing daily count exceeded"}`)
	}))
}

func servingServer(calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
}

func TestDailyCountSpentOnOneHostMovesToTheNext(t *testing.T) {
	var spentCalls, servedCalls atomic.Int32
	spent := dailySpentServer(&spentCalls)
	defer spent.Close()
	serving := servingServer(&servedCalls)
	defer serving.Close()
	request := upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}

	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, serving.URL, serving.URL, spent.URL)
	client.endpoints.inferenceFallbacks = []string{serving.URL}
	testutil.NoError(t, client.SendRequestWithPayload(context.Background(), request, nil, nil))
	testutil.Equal(t, spentCalls.Load(), int32(1))
	testutil.Equal(t, servedCalls.Load(), int32(1))

	// The account now skips the spent host instead of meeting the refusal on
	// every request.
	testutil.NoError(t, client.SendRequestWithPayload(context.Background(), request, nil, nil))
	testutil.Equal(t, spentCalls.Load(), int32(1))
	testutil.Equal(t, servedCalls.Load(), int32(2))

	// One account's spent count says nothing about the host: another account
	// still starts at the primary.
	other := signedTestAccount()
	other.ID = 2
	otherClient := NewFromAccount(other, nil)
	setTestEndpoints(otherClient, serving.URL, serving.URL, spent.URL)
	otherClient.endpoints.inferenceFallbacks = []string{serving.URL}
	testutil.NoError(t, otherClient.SendRequestWithPayload(context.Background(), request, nil, nil))
	testutil.Equal(t, spentCalls.Load(), int32(2))
}

func TestDailyCountSpentOnEveryHostReachesTheAccountPolicy(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	first := dailySpentServer(&firstCalls)
	defer first.Close()
	second := dailySpentServer(&secondCalls)
	defer second.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, first.URL, first.URL, first.URL)
	client.endpoints.inferenceFallbacks = []string{second.URL}
	err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil)
	testutil.False(t, !errors.Is(err, ErrDailyCountExceeded), "a count spent everywhere must reach the account policy")
	testutil.Equal(t, firstCalls.Load(), int32(1))
	testutil.Equal(t, secondCalls.Load(), int32(1))
}
