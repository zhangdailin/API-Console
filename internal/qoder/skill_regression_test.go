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

	"orchids-api/internal/upstream"
)

func TestSkillFinishReadsTail(t *testing.T) {
	for _, done := range []string{"data: [DONE]\n\n", envelope(`[DONE]`)} {
		prefix := envelope(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`) + done
		result, err := consumeStreamWithTools(strings.NewReader(prefix+envelope(`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7}}`)+"event:finish\ndata: {}\n\n"), false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.Usage["inputTokens"] != 11 {
			t.Fatalf("late usage lost: %#v", result.Usage)
		}
		_, err = consumeStreamWithTools(strings.NewReader(prefix+"event:error\ndata: failure\n\n"), false, nil)
		if err == nil {
			t.Fatal("late error lost")
		}
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
	if err := client.SendRequestWithPayload(context.Background(), upstream.UpstreamRequest{Model: "Qwen3.7-Max", Prompt: "hello"}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("attempts = %d", len(bodies))
	}
	if bodies[0].RequestID == bodies[1].RequestID || !bodies[1].IsRetry {
		t.Fatalf("retry reused request ID=%v; is_retry=%v", bodies[0].RequestID == bodies[1].RequestID, bodies[1].IsRetry)
	}
	if bodies[0].SessionID != bodies[1].SessionID {
		t.Fatal("local retry changed session")
	}
}

func TestSkillHTTPBusinessErrorsBeforeAuth(t *testing.T) {
	for _, status := range []int{401, 403} {
		err := classifyStatus(status, "", []byte(`{"message":"{\"agentLimitResetTime\":1790538433100}"}`))
		var limit *agentLimitError
		if !errors.As(err, &limit) || isUnauthorized(err) {
			t.Fatalf("allowance classified as %v", err)
		}
		err = classifyStatus(status, "", []byte(`{"message":"Duplicate request"}`))
		if isUnauthorized(err) || isRetryable(err) {
			t.Fatalf("duplicate classified as %v", err)
		}
	}
}

func TestSkillRequestControlsAndSharedRetry(t *testing.T) {
	maxTokens, temperature, topP := 123, 0.0, 0.25
	for _, attempt := range []int{0, 1, 2} {
		encoded, err := buildChatBody(upstream.UpstreamRequest{Prompt: "hello", Attempt: attempt, MaxTokens: &maxTokens, Temperature: &temperature, TopP: &topP, Stop: []string{"END"}}, modelEntry{Key: "test"}, "session", "request")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := decodeBody(encoded)
		if err != nil {
			t.Fatal(err)
		}
		var body chatBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if body.IsRetry != (attempt > 1) {
			t.Fatalf("attempt %d retry=%v", attempt, body.IsRetry)
		}
		p := body.Parameters
		if p["max_tokens"] != float64(123) || p["temperature"] != float64(0) || p["top_p"] != 0.25 {
			t.Fatalf("controls=%#v", p)
		}
		stops, ok := p["stop"].([]interface{})
		if !ok || len(stops) != 1 || stops[0] != "END" {
			t.Fatalf("stop=%#v", p["stop"])
		}
	}
}
