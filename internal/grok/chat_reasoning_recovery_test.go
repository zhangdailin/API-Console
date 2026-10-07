package grok

import (
	"encoding/base64"
	"net/http"
	"orchids-api/internal/testutil"
	"testing"
)

func validTestReplayCipher() string {
	data := make([]byte, 128)
	for i := range data {
		data[i] = byte(i)
	}
	return base64.RawStdEncoding.EncodeToString(data)
}

func TestReplayCipherValidation(t *testing.T) {
	testutil.False(t, !validReplayCipher(validTestReplayCipher()), "valid cipher rejected")
	for _, value := range []string{"cipher", "gAAAAsecret", " " + validTestReplayCipher(), validTestReplayCipher() + "=", base64.RawStdEncoding.EncodeToString(make([]byte, 128))} {
		testutil.False(t, validReplayCipher(value), "malformed cipher accepted")
	}
}

func TestChatReasoningRecoveryResetsOnlyAfterDecodeFailure(t *testing.T) {
	initial := newCLIUpstreamError(400, nil, []byte("invalid_encrypted_content"))
	payload := map[string]interface{}{"prompt_cache_key": "session", "input": []interface{}{map[string]interface{}{"type": "reasoning", "encrypted_content": "opaque", "summary": []interface{}{map[string]interface{}{"text": "remember this"}}}}}
	calls := 0
	response, err := recoverChatReasoning(payload, initial, func(stage string) (*http.Response, error) {
		calls++
		if calls == 1 {
			item := payload["input"].([]interface{})[0].(map[string]interface{})
			testutil.Fail(t, item["type"] != "message" || item["encrypted_content"] != nil, item)
			testutil.Equal(t, payload["prompt_cache_key"], "session")
			return nil, initial
		}
		testutil.Fail(t, payload["prompt_cache_key"] != nil || stage != "reasoning_session_reset", payload, stage)
		return &http.Response{StatusCode: 200}, nil
	})
	testutil.Fail(t, err != nil || response == nil || calls != 2, response, err, calls)
}

func TestChatReasoningRecoveryPreservesPreviousResponse(t *testing.T) {
	initial := newCLIUpstreamError(400, nil, []byte("invalid_encrypted_content"))
	payload := map[string]interface{}{"prompt_cache_key": "session", "previous_response_id": "resp_1"}
	_, err := recoverChatReasoning(payload, initial, func(string) (*http.Response, error) { t.Fatal("unsafe reset"); return nil, nil })
	testutil.Fail(t, err != initial || payload["prompt_cache_key"] != "session", err, payload)
}

func TestChatReasoningRecoveryDoesNotRetryRateLimit(t *testing.T) {
	initial := newCLIUpstreamError(400, nil, []byte("invalid_encrypted_content"))
	limited := newCLIUpstreamError(429, nil, []byte("rate limited"))
	payload := map[string]interface{}{"prompt_cache_key": "session", "input": []interface{}{map[string]interface{}{"type": "reasoning", "encrypted_content": "opaque"}}}
	calls := 0
	_, err := recoverChatReasoning(payload, initial, func(string) (*http.Response, error) { calls++; return nil, limited })
	testutil.Fail(t, err != limited || calls != 1, err, calls)
}
