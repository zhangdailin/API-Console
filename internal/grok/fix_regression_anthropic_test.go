package grok

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/chatwire"
	"strings"
	"testing"

	"encoding/json"

	"orchids-api/internal/testutil"
)

func TestAnthropicUsageCarriesCacheAndThinkingFields(t *testing.T) {
	usage := map[string]interface{}{
		"prompt_tokens":     100,
		"completion_tokens": 20,
		"prompt_tokens_details": map[string]interface{}{
			"cached_tokens":    30,
			"reasoning_tokens": 0,
		},
		"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 7},
	}
	got := anthropicUsageFromOpenAI(usage)
	testutil.Equal(t, got["input_tokens"], 70)
	testutil.Equal(t, got["cache_read_input_tokens"], 30)
	_, ok := got["cache_creation_input_tokens"]
	testutil.False(t, !ok, "cache_creation_input_tokens must be reported (0 is a value, not absence)")
	details, ok := got["output_tokens_details"].(map[string]interface{})
	if !ok || details["thinking_tokens"] != 7 {
		t.Fatalf("output_tokens_details = %v, want thinking_tokens=7", got["output_tokens_details"])
	}
}

func TestAnthropicRefusalUsesDedicatedStopReason(t *testing.T) {
	chat := map[string]interface{}{
		"id": "chatcmpl_1",
		"choices": []interface{}{map[string]interface{}{
			"finish_reason": "stop",
			"message":       map[string]interface{}{"refusal": "I can't help with that"},
		}},
	}
	got := anthropicResponseFromChat("grok-4.6", chat)
	testutil.Equal(t, got["stop_reason"], "refusal")
	if !strings.HasPrefix(fmt.Sprint(got["id"]), "msg_") {
		t.Fatalf("id = %v, want an Anthropic msg_ id, not a chatcmpl_ id", got["id"])
	}
}

func TestOpenAIFinishToAnthropicMapsRefusal(t *testing.T) {
	testutil.Equal(t, openAIFinishToAnthropic("content_filter"), "refusal")
	testutil.Equal(t, openAIFinishToAnthropic("stop"), "end_turn")
}

func TestAnthropicMessageIDReshapesChatCompletionsID(t *testing.T) {
	testutil.Equal(t, anthropicMessageID("chatcmpl_abc"), "msg_abc")
	testutil.Equal(t, anthropicMessageID("msg_keep"), "msg_keep")
	got := anthropicMessageID("")
	testutil.Falsef(t, !strings.HasPrefix(got, "msg_") || len(got) != len("msg_")+24, "empty id -> %q, want a generated msg_ id", got)
}

func TestPrepareGrokSessionRecognizesAgentSessionHeaders(t *testing.T) {
	base := []chatwire.Message{{Role: "user", Content: "hello"}}
	cases := map[string]string{
		"X-Claude-Code-Session-Id": "claude-sid",
		"X-Codex-Session-Id":       "codex-sid",
		"X-Codex-Conversation-Id":  "codex-conv",
		"X-Grok-Session-Id":        "grok-sid",
		"X-Session-Id":             "plain-sid",
	}
	for header, value := range cases {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set(header, value)
		session := prepareGrokSession(req, "grok-4.6", "", base)
		testutil.NotEqual(t, session.Key, "")
		// An explicit client identity permits encrypted reasoning replay; the
		// message-prefix fallback is affinity-only.
		testutil.Falsef(t, !session.Replay, "%s: session is not marked replay-capable", header)
	}
	// Two different clients using the same identifier must not collide.
	reqA := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqA.Header.Set("X-Claude-Code-Session-Id", "shared")
	reqB := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	reqB.Header.Set("X-Codex-Session-Id", "shared")
	a, b := prepareGrokSession(reqA, "grok-4.6", "", base), prepareGrokSession(reqB, "grok-4.6", "", base)
	testutil.False(t, a.Key == b.Key, "identical seeds from different clients collided")
	// Without any client identity the fallback is affinity-only.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	fallback := prepareGrokSession(req, "grok-4.6", "", base)
	testutil.False(t, fallback.Replay, "a message-prefix fallback must not enable reasoning replay")
}

func TestAnthropicErrorTypeFollowsStatus(t *testing.T) {
	cases := map[int]string{
		http.StatusBadRequest:         "invalid_request_error",
		http.StatusUnauthorized:       "authentication_error",
		http.StatusForbidden:          "permission_error",
		http.StatusNotFound:           "not_found_error",
		http.StatusTooManyRequests:    "rate_limit_error",
		http.StatusServiceUnavailable: "overloaded_error",
	}
	for status, want := range cases {
		rec := httptest.NewRecorder()
		writeAnthropicError(rec, status, "boom")
		var envelope struct {
			Type  string `json:"type"`
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		err := json.Unmarshal(rec.Body.Bytes(), &envelope)
		testutil.CheckNoError(t, err)
		testutil.Equal(t, envelope.Error.Type, want)
		testutil.NotEqual(t, envelope.Error.Code, "")
	}
}
