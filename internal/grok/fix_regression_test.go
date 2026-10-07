package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// The client-facing error object has to be parseable by an OpenAI SDK: the
// handler used to answer text/plain, so resp.json()["error"] threw before the
// caller could read the reason.
func TestResponsesUpstreamFailureMapsAuthAndRetryAfter(t *testing.T) {
	err := newCLIUpstreamError(http.StatusUnauthorized, http.Header{"Retry-After": {"7"}}, []byte(`{"error":"expired"}`))
	rec := httptest.NewRecorder()
	writeGrokUpstreamFailure(rec, http.StatusUnauthorized, err)
	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)
	testutil.Equal(t, rec.Header().Get("Retry-After"), "7")
}

func TestSyntheticCooldownCarriesTypedHint(t *testing.T) {
	err := newSyntheticCooldownError("build:team:t", "grok-4.6", 12*time.Second)
	var hinted interface{ RetryAfter() time.Duration }
	testutil.Falsef(t, !errors.As(err, &hinted) || hinted.RetryAfter() != 12*time.Second, "typed cooldown hint missing: %v", err)
	testutil.Falsef(t, !isSharedGrokRateLimitError(err) || markAllGrokAccountStatuses(err) || !shouldSwitchGrokAccount(err), "synthetic cooldown policy mismatch: %v", err)
	var upstream *grokUpstreamError
	testutil.True(t, errors.As(err, &upstream), "synthetic cooldown lost HTTP evidence")
	testutil.Equal(t, upstreamStatus(err), http.StatusTooManyRequests)
	testutil.Equal(t, ClassifyUpstreamError(err), UpstreamErrorRateLimited)
	testutil.Equal(t, upstreamRetryAfterSeconds(err), 12)
	testutil.True(t, isUpstreamFailure(err), "synthetic cooldown was classified as local")
}

func TestReadAndValidateNativeResponseBeforeCommit(t *testing.T) {
	_, err := readAndValidateNativeResponse(strings.NewReader(`not-json`))
	testutil.Error(t, err)
	raw, err := readAndValidateNativeResponse(strings.NewReader(`{"id":"resp_1","status":"completed"}`))
	testutil.Falsef(t, err != nil || len(raw) == 0, "valid response rejected: %v", err)
}

// TestGrokErrorEnvelope pins the shared OpenAI error object for every status: the
// content type an SDK needs, the status, the type derived from it and the
// machine-readable code.
func TestGrokErrorEnvelope(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		message     string
		wantType    string
		wantCode    string
		wantMessage string
	}{
		{name: "bad request keeps its own message", status: http.StatusBadRequest, message: "messages is required", wantType: "invalid_request_error", wantCode: "invalid_request", wantMessage: "messages is required"},
		{name: "unauthorized", status: http.StatusUnauthorized, message: "boom", wantType: "authentication_error", wantCode: "invalid_api_key"},
		{name: "rate limited", status: http.StatusTooManyRequests, message: "boom", wantType: "rate_limit_error", wantCode: "rate_limit_exceeded"},
		{name: "service unavailable", status: http.StatusServiceUnavailable, message: "boom", wantType: "server_error", wantCode: "service_unavailable"},
		{name: "entity too large", status: http.StatusRequestEntityTooLarge, message: "boom", wantType: "invalid_request_error", wantCode: "request_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGrokError(rec, tc.status, tc.message)
			testutil.Equal(t, rec.Code, tc.status)
			got := rec.Header().Get("Content-Type")
			testutil.Falsef(t, !strings.HasPrefix(got, "application/json"), "Content-Type = %q, want application/json", got)
			var body struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
					Code    string `json:"code"`
					Param   any    `json:"param"`
				} `json:"error"`
			}
			err := json.Unmarshal(rec.Body.Bytes(), &body)
			testutil.CheckNoError(t, err)
			testutil.Equal(t, body.Error.Type, tc.wantType)
			testutil.Equal(t, body.Error.Code, tc.wantCode)
			testutil.Falsef(t, tc.wantMessage != "" && body.Error.Message != tc.wantMessage, "message = %q, want %q", body.Error.Message, tc.wantMessage)
		})
	}
}

func TestWriteGrokModelNotFoundReturns404Code(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGrokErrorCode(rec, http.StatusNotFound, "model_not_found", modelNotFoundMessage("missing"))
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	var body map[string]map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), "invalid JSON: %v")
	testutil.Equal(t, fmt.Sprint(body["error"]["code"]), "model_not_found")
}

// An upstream failure must never hand the caller the upstream body or the
// internal "status=… body=…" shape.
func TestWriteGrokUpstreamErrorSanitizesInternalDetail(t *testing.T) {
	// The typed error is assembled directly so the internal text can be
	// inspected. The production constructor never exposes this shape to a
	// client, and the response must not echo it when it is attached.
	upstream := &grokUpstreamError{
		status: http.StatusUnauthorized,
		header: sanitizeUpstreamHeader(http.Header{"Retry-After": {"7"}}),
		body:   boundedUpstreamBody([]byte(`{"error":{"message":"account team=acme quota exhausted; upgrade at x.ai/pricing"}}`)),
	}
	// The internal text must carry every detail the leak check below looks for,
	// otherwise that check proves nothing.
	if text := upstream.Error(); !strings.Contains(text, "acme") ||
		!strings.Contains(text, "status=") || !strings.Contains(text, "body=") {
		t.Fatalf("the internal error text is missing detail the leak check below needs: %q", text)
	}
	rec := httptest.NewRecorder()
	writeGrokUpstreamError(rec, upstream)

	testutil.Equal(t, rec.Code, http.StatusServiceUnavailable)
	testutil.Equal(t, rec.Header().Get("Retry-After"), "7")
	body := rec.Body.String()
	for _, leak := range []string{"acme", "x.ai/pricing", "status=", "body="} {
		testutil.MustNotContain(t, body, leak)
	}
}

// A local validation error is not an upstream failure: preserve its own
// message and 400 status rather than replacing it with a generic 503.
func TestWriteGrokUpstreamErrorKeepsLocalValidationMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	writeGrokUpstreamError(rec, errors.New("missing model"))
	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	testutil.MustContain(t, rec.Body.String(), "missing model")
}

func TestIsPrivateBuildControlEvent(t *testing.T) {
	testutil.False(t, !isPrivateBuildControlEvent("response.doom_loop_check"), "doom loop control event must be treated as private")
	testutil.False(t, isPrivateBuildControlEvent("response.output_text.delta"), "generated delta must not be treated as private")
}

func TestIsModelScopedRefusal(t *testing.T) {
	for _, body := range []string{
		`{"error":"access to the chat endpoint is denied"}`,
		"model is not available",
		`{"message":"not available for model grok-4.6"}`,
	} {
		err := newCLIUpstreamError(403, nil, []byte(body))
		testutil.True(t, isModelScopedRefusal(err), "model refusal not recognized")
	}
	for _, err := range []error{
		newCLIUpstreamError(403, nil, []byte("account banned")),
		newCLIUpstreamError(401, nil, []byte("unauthorized")),
		newCLIUpstreamError(429, nil, []byte("slow down")),
	} {
		testutil.False(t, isModelScopedRefusal(err), "account refusal mistaken for model refusal")
	}
}

func TestModelScopedFreeQuotaRefusal(t *testing.T) {
	testutil.False(t, !modelScopedFreeQuotaRefusal([]byte("You've used all the included free usage for model grok-4.6.")), "model-scoped free usage refusal not detected")
	testutil.False(t, modelScopedFreeQuotaRefusal([]byte("subscription:free-usage-exhausted")), "account-scoped refusal must not be treated as model-scoped")
}

func TestAnthropicUpstreamErrorDoesNotLeakUpstreamBody(t *testing.T) {
	const upstream = `{"error":"account team=acme-team quota exhausted; visit x.ai/pricing"}`
	rec := httptest.NewRecorder()
	writeAnthropicUpstreamError(rec, http.StatusBadRequest, upstream)
	body := rec.Body.String()
	for _, leak := range []string{"acme-team", "x.ai/pricing", "quota exhausted"} {
		testutil.MustNotContain(t, body, leak)
	}
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &envelope)
	testutil.CheckNoError(t, err)
	testutil.Falsef(t, envelope.Type != "error" || envelope.Error.Message == "", "unexpected Anthropic error envelope: %+v", envelope)
}

func TestIdleTimeoutIsClassifiedSeparately(t *testing.T) {
	// The sentinel is exported so every plane can recognise the condition.
	testutil.False(t, !errors.Is(errGrokSemanticIdle, ErrGrokSemanticIdle), "the ported alias must resolve to the exported sentinel")
	code, message := classifySynthesizedFailure("stream_read_error", "stream read error", errGrokSemanticIdle)
	testutil.Equal(t, code, "upstream_stream_idle_timeout")
	testutil.MustNotContain(t, message, "parse")
	// Any other failure keeps its own classification.
	code, _ = classifySynthesizedFailure("stream_read_error", "stream read error", errors.New("boom"))
	testutil.Equal(t, code, "stream_read_error")
}

func TestResponsesAPIErrorTypeFollowsStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	writeResponsesAPIError(rec, http.StatusServiceUnavailable, "service_unavailable", "busy")
	var envelope struct {
		Error struct {
			Type  string `json:"type"`
			Code  string `json:"code"`
			Param any    `json:"param"`
		} `json:"error"`
	}
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), "invalid JSON: %v")
	testutil.Equal(t, envelope.Error.Type, "server_error")
	rec = httptest.NewRecorder()
	writeResponsesAPIError(rec, http.StatusTooManyRequests, "rate_limit_exceeded", "slow down")
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope), "invalid JSON: %v")
	testutil.Equal(t, envelope.Error.Type, "rate_limit_error")
}

func TestBuildSessionUUIDIsStableAndValid(t *testing.T) {
	first := buildSessionUUID("deadbeef")
	testutil.True(t, isUUID(first), "buildSessionUUID() = %q, want a UUID")
	testutil.Equal(t, first, buildSessionUUID("deadbeef"))
	testutil.NotEqual(t, first, buildSessionUUID("deadbeee"))
	existing := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	testutil.Equal(t, buildSessionUUID(existing), existing)
}

func TestToolMessagesRequireCallID(t *testing.T) {
	messages := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: map[string]interface{}{"name": "read", "arguments": "{}"}}}},
		{Role: "tool", Name: "read", Content: "done"},
	}
	items, _ := responsesInputFromChatMessages(messages)
	for _, item := range items {
		m, ok := item.(map[string]interface{})
		testutil.Falsef(t, ok && m["type"] == "function_call_output", "a tool message without tool_call_id must not become a function_call_output: %#v", m)
	}
}

func TestChatToolUseMustBeAnswered(t *testing.T) {
	unanswered := []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Function: map[string]interface{}{"name": "read"}}}},
		{Role: "user", Content: "next"},
	}
	err := validateChatToolSequence(unanswered)
	testutil.Error(t, err)
	answered := append([]ChatMessage{}, unanswered[0], ChatMessage{Role: "tool", ToolCallID: "call_1", Content: "ok"})
	testutil.NoError(t, validateChatToolSequence(answered), "a paired tool_use must be accepted: %v")
}

func TestBackfillReasoningForCalls(t *testing.T) {
	proof := map[string]interface{}{"type": "reasoning", "id": "rs_1", "encrypted_content": "cipher-1"}
	cached := []interface{}{
		proof,
		map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": "{}"},
	}
	// The client echoes the call but not its proof.
	input := []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_1", "name": "read", "arguments": "{}"}}
	filled := backfillReasoningForCalls(input, cached)
	testutil.Equal(t, len(filled), 2)
	first, _ := filled[0].(map[string]interface{})
	testutil.Equal(t, first["type"], "reasoning")
	testutil.Equal(t, first["encrypted_content"], "cipher-1")
	// A call that already carries its proof is not doubled.
	withProof := append(cloneReplayItems([]interface{}{proof}), input...)
	got := backfillReasoningForCalls(withProof, cached)
	testutil.Falsef(t, len(got) != len(withProof), "a call that already carries its proof must not be doubled: %#v", got)
	// An unknown call id is left alone.
	unknown := []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_9", "name": "read"}}
	got = backfillReasoningForCalls(unknown, cached)
	testutil.Falsef(t, len(got) != 1, "an unknown call must not gain a proof: %#v", got)
	// No cache means no change.
	plain := []interface{}{map[string]interface{}{"type": "function_call", "call_id": "call_1"}}
	got = backfillReasoningForCalls(plain, nil)
	testutil.Falsef(t, len(got) != 1, "without cached items nothing may be inserted: %#v", got)
}

func TestReasoningForCallsIndexesOnlyProofs(t *testing.T) {
	index := reasoningForCalls([]interface{}{
		map[string]interface{}{"type": "reasoning", "id": "rs_1"}, // no encrypted content
		map[string]interface{}{"type": "function_call", "call_id": "call_1"},
		map[string]interface{}{"type": "reasoning", "id": "rs_2", "encrypted_content": "cipher"},
		map[string]interface{}{"type": "custom_tool_call", "call_id": "call_2"},
	})
	_, ok := index["call_1"]
	testutil.False(t, ok, "a reasoning item without a proof must not be indexed")
	entry, ok := index["call_2"]
	testutil.Falsef(t, !ok || entry["id"] != "rs_2", "call_2 index = %#v, want rs_2", entry)
}

// TestAccumulatedInputItemsWalksTheContinuationChain drives the real chain
// builder: ancestor inputs are appended nearest-first, the walk stops at the
// documented bound, and a cycle terminates instead of looping forever.
func TestAccumulatedInputItemsWalksTheContinuationChain(t *testing.T) {
	t.Parallel()
	testutil.Falsef(t, maxStoredInputChainDepth < 1 || maxStoredInputChainDepth > 64, "chain depth = %d, want a bounded positive value no greater than 64", maxStoredInputChainDepth)

	s := newTestGrokStore(t, "chain:")
	h := NewHandler(nil, loadbalancer.NewWithCacheTTL(s, 0))

	ctx := context.Background()
	save := func(id, previous, text string) {
		raw, err := json.Marshal([]map[string]string{{"role": "user", "content": text}})
		testutil.Falsef(t, err != nil, "marshal %s: %v", id, err)
		if err := s.SaveStoredResponse(ctx, &store.StoredResponse{
			ResponseID:         id,
			OwnerHash:          "owner",
			PreviousResponseID: previous,
			InputItems:         raw,
		}, time.Hour); err != nil {
			t.Fatalf("SaveStoredResponse(%s) error = %v", id, err)
		}
	}

	// A cycle: the walk must stop on the seen set.
	save("resp_a", "resp_b", "a")
	save("resp_b", "resp_a", "b")

	// A chain longer than the bound: only the nearest ancestors are folded in.
	const chainLength = maxStoredInputChainDepth + 4
	for i := 0; i <= chainLength; i++ {
		previous := ""
		if i < chainLength {
			previous = fmt.Sprintf("resp_c%d", i+1)
		}
		save(fmt.Sprintf("resp_c%d", i), previous, fmt.Sprintf("c%d", i))
	}

	itemText := func(item interface{}) string {
		entry, ok := item.(map[string]interface{})
		testutil.True(t, ok, "input item = %#v, want an object")
		return fmt.Sprint(entry["content"])
	}

	cases := []struct {
		name      string
		previous  string
		wantOrder []string
	}{
		{name: "cycle terminates", previous: "resp_a", wantOrder: []string{"current", "a", "b"}},
		{name: "bound keeps the nearest ancestors", previous: "resp_c0", wantOrder: append([]string{"current"}, func() []string {
			ids := make([]string, 0, maxStoredInputChainDepth)
			for i := 0; i < maxStoredInputChainDepth; i++ {
				ids = append(ids, fmt.Sprintf("c%d", i))
			}
			return ids
		}()...)},
		{name: "unknown ancestor adds nothing", previous: "resp_missing", wantOrder: []string{"current"}},
		{name: "no ancestor", previous: "", wantOrder: []string{"current"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"input":                []interface{}{map[string]interface{}{"role": "user", "content": "current"}},
				"previous_response_id": tc.previous,
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			got := h.accumulatedInputItems(req, "owner", payload)
			testutil.Equal(t, len(got), len(tc.wantOrder))
			for i, want := range tc.wantOrder {
				testutil.Equal(t, itemText(got[i]), want)
			}
		})
	}
}
