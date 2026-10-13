package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

func TestSkillFinishReadsTail(t *testing.T) {
	for _, done := range []string{"data: [DONE]\n\n", envelope(`[DONE]`)} {
		prefix := envelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`) + done
		result, err := consumeStreamObserved(strings.NewReader(prefix+envelope(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)+"event:finish\ndata: {}\n\n"), nil, nil)
		testutil.NoError(t, err)
		testutil.Equal(t, result.Usage["inputTokens"], 11)
		_, err = consumeStreamObserved(strings.NewReader(prefix+"event:error\ndata: failure\n\n"), nil, nil)
		testutil.False(t, err == nil, "late error lost")
	}
}

func TestSkillTransientFreshIdentity(t *testing.T) {
	var bodies []chatBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoded, _ := io.ReadAll(r.Body)
		raw, err := decodeBody(encoded)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var body chatBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			w.WriteHeader(500)
			_, _ = io.WriteString(w, `{"message":"provider_error"}`)
			return
		}
		_, _ = io.WriteString(w, envelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`)+"event:finish\ndata: {}\n\n")
	}))
	defer server.Close()
	client := NewFromAccount(signedTestAccount(), nil)
	setTestEndpoints(client, server.URL, server.URL, server.URL)
	testutil.NoError(t, client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil))
	testutil.Equal(t, len(bodies), 2)
	if bodies[0].RequestID == bodies[1].RequestID || bodies[1].IsRetry {
		t.Fatalf("retry reused request ID=%v; is_retry=%v", bodies[0].RequestID == bodies[1].RequestID, bodies[1].IsRetry)
	}
	testutil.Equal(t, bodies[0].SessionID, bodies[1].SessionID)
}

func TestSkillHTTPBusinessErrorsBeforeAuth(t *testing.T) {
	for _, status := range []int{401, 403} {
		err := classifyStatus(status, "", []byte(`{"message":"{\"agentLimitResetTime\":1790538433100}"}`))
		var limit *agentLimitError
		testutil.Falsef(t, !errors.As(err, &limit) || isUnauthorized(err), "allowance classified as %v", err)
		err = classifyStatus(status, "", []byte(`{"message":"Duplicate request"}`))
		testutil.Falsef(t, isUnauthorized(err) || isRetryable(err), "duplicate classified as %v", err)
	}
}

func TestSkillRequestControlsAndSharedRetry(t *testing.T) {
	maxTokens, temperature, topP := 123, 0.0, 0.25
	for _, attempt := range []int{0, 1, 2} {
		encoded, err := buildChatBodyProfile(upstream.UpstreamRequest{Prompt: "hello", Attempt: attempt, MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP, Stop: []string{"END"}}, modelEntry{Key: "test"}, "session", "request", "request-set", DefaultClientVersion, "", sceneBusinessProduct)
		testutil.NoError(t, err)
		raw, err := decodeBody(encoded)
		testutil.NoError(t, err)
		var body chatBody
		testutil.NoError(t, json.Unmarshal(raw, &body))
		testutil.Falsef(t, body.IsRetry, "attempt %d retry=%v", attempt, body.IsRetry)
		p := body.Parameters.(map[string]interface{})
		testutil.Falsef(t, p["max_tokens"] != float64(123) || p["temperature"] != float64(0) || p["top_p"] != 0.25, "controls=%#v", p)
		stops, ok := p["stop"].([]interface{})
		if !ok || len(stops) != 1 || stops[0] != "END" {
			t.Fatalf("stop=%#v", p["stop"])
		}
	}
}
