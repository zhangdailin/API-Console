package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type countingFailUpstream struct {
	err   error
	calls *atomic.Int32
}

func (m *countingFailUpstream) SendRequestWithPayload(context.Context, upstream.UpstreamRequest, func(upstream.SSEMessage), *debug.Logger) error {
	m.calls.Add(1)
	return m.err
}

// refusalPool builds a pool of n accounts of one type whose upstream always
// fails with err, sends one request, and returns how many upstream calls it
// made and which accounts ended up with a status.
func refusalPool(t *testing.T, accountType, channel, path string, n int, err error) (int32, map[int64]string, int) {
	t.Helper()
	s, _ := setupConnTrackerHandlerTest(t)
	publishModel(t, s, &store.Model{Channel: channel, ModelID: "model-x"})
	var ids []int64
	for i := range n {
		acc := createEnabledTestAccount(t, s, accountType+"-"+string(rune('a'+i)), accountType)
		ids = append(ids, acc.ID)
	}
	lb := loadbalancer.NewWithCacheTTL(s, time.Second)
	h := NewWithLoadBalancer(&config.Config{RequestTimeout: 10, MaxRetries: 20}, lb)
	calls := &atomic.Int32{}
	h.SetClientFactory(func(*store.Account, *config.Config) UpstreamClient {
		return &countingFailUpstream{err: err, calls: calls}
	})
	body, _ := json.Marshal(map[string]any{"model": "model-x", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "stream": false})
	rec := httptest.NewRecorder()
	h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, "http://x"+path, bytes.NewReader(body)))

	statuses := map[int64]string{}
	for _, id := range ids {
		acc, getErr := s.GetAccount(context.Background(), id)
		testutil.NoError(t, getErr)
		if acc.StatusCode != "" {
			statuses[id] = acc.StatusCode
		}
	}
	return calls.Load(), statuses, rec.Code
}

// An identical 403 from a second account is the request's own: one account at
// most may be marked, never the pool.
func TestIdenticalRefusalDoesNotSpreadAcrossThePool(t *testing.T) {
	calls, statuses, code := refusalPool(t, "workbuddy", "WorkBuddy", "/workbuddy/v1/messages", 5, errors.New("HTTP 403 Forbidden"))
	testutil.Equal(t, calls, int32(2))
	testutil.Equal(t, len(statuses), 1)
	testutil.NotEqual(t, code, http.StatusOK)
}

// The model provider's refusal behind Cline marks nothing and is not retried.
func TestClineModelProviderRefusalMarksNoAccount(t *testing.T) {
	refusal := errors.New(`cline stream error: Failed to create stream: inference request failed: failed to generate stream from Vercel: failed to invoke model 'model-x' with streaming: request failed with status 403: {"error":{"message":"Forbidden","type":"AI_APICallError"}}`)
	calls, statuses, code := refusalPool(t, "cline", "Cline", "/cline/v1/messages", 5, refusal)
	testutil.Equal(t, calls, int32(1))
	testutil.Equal(t, len(statuses), 0)
	testutil.NotEqual(t, code, http.StatusOK)
}
