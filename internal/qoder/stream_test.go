package qoder

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"encoding/json"

	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
	"orchids-api/internal/util"
)

// envelope wraps one inner chunk the way the upstream does: a JSON object whose
// `body` field is a JSON string.
func envelope(inner string) string {
	return "data: " + `{"statusCodeValue":200,"body":` + jsonString(inner) + "}\n\n"
}

func jsonString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\n':
			builder.WriteString(`\n`)
		default:
			builder.WriteRune(r)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// collectStream runs the parser and returns the events it produced.
func collectStream(t *testing.T, body string) ([]upstream.SSEMessage, streamResult, error) {
	t.Helper()
	var events []upstream.SSEMessage
	result, err := consumeStreamObserved(strings.NewReader(body), func(msg upstream.SSEMessage) {
		events = append(events, msg)
	}, nil)
	return events, result, err
}

// TestConsumeStreamDecodesWrappedChunks pins the double unwrapping: reading the
// envelope as the chunk yields an empty answer, which is indistinguishable from
// a model that returned nothing.
func TestConsumeStreamDecodesWrappedChunks(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	testutil.NoError(t, err, "consumeStream() error = %v")

	var text strings.Builder
	var reasoning strings.Builder
	for _, event := range events {
		switch event.Type {
		case "model.text-delta":
			text.WriteString(event.Event["delta"].(string))
		case "model.reasoning-delta":
			reasoning.WriteString(event.Event["delta"].(string))
		}
	}
	testutil.Equal(t, text.String(), "Hello")
	testutil.Equal(t, reasoning.String(), "think")
	testutil.Equal(t, result.FinishReason(), "end_turn")
	testutil.False(t, !result.SawMeaningfulEvent, "SawMeaningfulEvent = false")
}

func TestConsumeStreamClassifiesTextRateLimit(t *testing.T) {
	t.Parallel()
	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"The available upstream accounts are rate-limited. Retry after the cooldown. Request ID: abc"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	events, _, err := collectStream(t, body)
	testutil.Falsef(t, !errors.Is(err, ErrModelRateLimited), "error = %v, want ErrModelRateLimited", err)
	testutil.Equal(t, len(events), 0)
}

// TestConsumeStreamRequiresTerminator proves a premature EOF is reported as a
// truncation instead of as a successful short answer. Silently accepting it
// would hand the client a cut-off response with no error.
func TestConsumeStreamRequiresTerminator(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)
	_, result, err := collectStream(t, body)
	testutil.Falsef(t, !errors.Is(err, ErrStreamTruncated), "error = %v, want ErrStreamTruncated", err)
	testutil.False(t, !result.SawMeaningfulEvent, "SawMeaningfulEvent = false, want the partial content to be recorded")
}

// A done marker without Qoder's final event must not hide a truncated tail.
func TestConsumeStreamBareDoneRequiresFinish(t *testing.T) {
	t.Parallel()
	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"ok"}}]}`) + "data: [DONE]\n\n"
	_, _, err := collectStream(t, body)
	testutil.Falsef(t, !errors.Is(err, ErrStreamTruncated), "error = %v, want ErrStreamTruncated", err)
}

func TestConsumeStreamEnvelopeDoneRequiresFinish(t *testing.T) {
	t.Parallel()
	_, _, err := collectStream(t, envelope(`[DONE]`))
	testutil.Falsef(t, !errors.Is(err, ErrStreamTruncated), "error = %v, want ErrStreamTruncated", err)
}

// TestConsumeStreamReportsErrorEnvelope proves an upstream failure is surfaced
// rather than being swallowed as an empty stream.
func TestConsumeStreamReportsErrorEnvelope(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":500,"body":"{\"message\":\"model overloaded\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	testutil.False(t, err == nil, "consumeStream() error = nil for an error envelope")
	testutil.MustContain(t, err.Error(), "model overloaded")
}

// TestConsumeStreamClassifiesBusyCode proves business code 10605 is reported as
// a queue refusal under a 401, because refreshing the token cannot fix it and
// the retry policy differs.
func TestConsumeStreamClassifiesSplitTextRateLimit(t *testing.T) {
	t.Parallel()
	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"The available upstream accounts are rate-"}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"limited. Retry later"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	events, _, err := collectStream(t, body)
	testutil.Falsef(t, !errors.Is(err, ErrModelRateLimited), "error = %v, want ErrModelRateLimited", err)
	testutil.Equal(t, len(events), 0)
}

func TestConsumeStreamClassifiesBusyCode(t *testing.T) {
	t.Parallel()

	for _, bodyJSON := range []string{
		`{"code":"10605","message":"queue full"}`,
		`{"code":10605,"message":"{\"isQueued\":true,\"retryAfterSeconds\":29,\"serviceAvailable\":false,\"waitTime\":29}"}`,
	} {
		body := "data: " + `{"statusCodeValue":401,"body":` + strconv.Quote(bodyJSON) + `}` + "\n\n"
		_, _, err := collectStream(t, body)
		testutil.Falsef(t, !errors.Is(err, ErrBusy), "error = %v, want ErrBusy for %s", err, bodyJSON)
		testutil.Falsef(t, errors.Is(err, errUpstreamUnauthorized), "busy refusal was misclassified as unauthorized: %v", err)
		var attemptErr *attemptStreamError
		testutil.Falsef(t, !errors.As(err, &attemptErr) || !attemptErr.busy || !attemptErr.retryable, "busy refusal lacks typed retry metadata: %#v", err)
		testutil.Falsef(t, strings.Contains(bodyJSON, "retryAfterSeconds") && attemptErr.RetryAfter() != 29*time.Second, "RetryAfter=%v want 29s", attemptErr.RetryAfter())
	}
}

func TestConsumeStreamPreservesEnvelopeStatusForClassification(t *testing.T) {
	t.Parallel()
	body := "data: " + `{"statusCodeValue":400,"body":"{\"message\":\"invalid tool schema\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "status=400"), "error = %v, want explicit status=400", err)
}

func TestConsumeStreamClassifiesAgentLimitWithoutClaimingAccountQuota(t *testing.T) {
	t.Parallel()
	const resetMillis = int64(1790538433100)
	body := "data: " + `{"statusCodeValue":401,"body":"{\"message\":\"{\\\"agentLimitResetTime\\\":1790538433100}\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	var agentErr *agentLimitError
	testutil.Falsef(t, !errors.As(err, &agentErr), "error = %v, want agentLimitError", err)
	want := time.UnixMilli(resetMillis)
	testutil.Falsef(t, !agentErr.resetAt.Equal(want), "reset=%v want %v", agentErr.resetAt, want)
	testutil.MustNotContain(t, err.Error(), "quota exhausted")
}

func TestConsumeStreamDoesNotRetryDuplicateRequest(t *testing.T) {
	t.Parallel()
	body := "data: " + `{"statusCodeValue":401,"body":"{\"message\":\"Duplicate request\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "duplicate request") || errors.Is(err, errUpstreamUnauthorized), "error = %v, want non-auth duplicate request", err)
}

// TestConsumeStreamAuthenticatedRequestEnvelope covers the exact answer a live
// account without a subscription receives: HTTP 200, a business status of 403,
// and a body that names the pricing page.
//
// It must NOT be reported as an authentication failure: the credential was
// accepted, and classifying it as unauthorized makes the shared account
// classifier retire a working account as "forbidden" — the misdiagnosis this
// test exists to prevent.
func TestConsumeStreamAuthenticatedRequestEnvelope(t *testing.T) {
	t.Parallel()

	// The body is exactly as the gateway sends it, including the escaping.
	body := "data:{\"headers\":{\"Content-Type\":[\"application/json\"]},\"body\":\"{\\\"code\\\":\\\"112\\\",\\\"message\\\":\\\"{\\\\\\\"pricingUrl\\\\\\\":\\\\\\\"https://qoder.com/pricing?client=qoder\\\\\\\"}\\\"}\",\"statusCodeValue\":403,\"statusCode\":\"FORBIDDEN\"}\n\n"

	_, _, err := collectStream(t, body)
	testutil.False(t, err == nil, "consumeStream() error = nil for an entitlement refusal")
	testutil.Falsef(t, !errors.Is(err, ErrNoEntitlement), "error = %v, want ErrNoEntitlement", err)
	// The account classifier reads any "status=403" as a dead credential, so the
	// error text must not carry the upstream status.
	testutil.MustNotContain(t, err.Error(), "status=403")
	testutil.MustNotContain(t, err.Error(), "forbidden")
	// The reason must reach the operator.
	testutil.Falsef(t, !strings.Contains(err.Error(), "pricing") && !strings.Contains(err.Error(), "plan"), "error text = %q, want the entitlement reason", err)
}

// TestConsumeStreamClassifiesUnauthorizedEnvelope proves a genuine auth failure
// is distinguished from a busy verdict.
func TestConsumeStreamClassifiesUnauthorizedEnvelope(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":403,"body":"{\"message\":\"login expired\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	testutil.Falsef(t, !errors.Is(err, errUpstreamUnauthorized), "error = %v, want errUpstreamUnauthorized", err)
	testutil.Falsef(t, !strings.Contains(err.Error(), "status=403") && !strings.Contains(err.Error(), "login expired"), "error = %v, want the upstream reason", err)
}

// TestConsumeStreamRejectsMalformedFrames proves corrupt stream data cannot be
// silently omitted from an otherwise successful answer.
func TestConsumeStreamRejectsMalformedFrames(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"before"}}]}`) +
		"data: not-json\n\n" +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"after"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"

	_, _, err := collectStream(t, body)
	testutil.Falsef(t, err == nil || !strings.Contains(err.Error(), "protocol error"), "consumeStream() error = %v, want protocol error", err)
}

// TestConsumeStreamUsageSurvivesASharedFrame proves usage is captured even when
// it rides along with the final content frame, which is how the gateway reports
// the last chunk.
func TestConsumeStreamUsageSurvivesASharedFrame(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"final"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3,"cacheable_tokens":5},"completion_tokens_details":{"reasoning_tokens":2},"credits":0.25,"original_credits":0.5}}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	testutil.NoError(t, err, "consumeStream() error = %v")
	var sawUsage bool
	var sawText bool
	for _, event := range events {
		switch event.Type {
		case "model.tokens-used":
			sawUsage = true
		case "model.text-delta":
			sawText = true
		}
	}
	testutil.True(t, sawText, "the content in a shared usage frame was dropped")
	testutil.True(t, sawUsage, "usage was not emitted")
	testutil.Equal(t, result.Usage["inputTokens"], 11)
	testutil.Equal(t, result.Usage["outputTokens"], 7)
	testutil.Equal(t, result.Usage["cacheReadTokens"], 3)
	testutil.Equal(t, result.Usage["cacheable_tokens"], 5)
	testutil.Equal(t, result.Usage["credits"], 0.25)
}

// TestConsumeStreamToolCallAccumulatesArguments proves split argument deltas are
// reassembled. Emitting on the first delta loses every later fragment and
// produces invalid JSON.
func TestConsumeStreamToolCallAccumulatesArguments(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"cats\"}"}}]}}]}`) +
		envelope(`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	testutil.NoError(t, err, "consumeStream() error = %v")
	var call map[string]interface{}
	for _, event := range events {
		if event.Type == "model.tool-call" {
			call = event.Event
		}
	}
	testutil.Falsef(t, call == nil, "no tool call was emitted; events = %+v", events)
	testutil.Equal(t, call["toolName"], "search")
	testutil.Equal(t, call["input"], `{"q":"cats"}`)
	testutil.Equal(t, result.FinishReason(), "tool_use")
	testutil.Equal(t, result.ToolCallCount, 1)
}

// TestToolCallAccumulatorOpensNewCallOnIDChange covers parallel calls, which the
// upstream sometimes reports by reusing one index and changing only the id.
//
// Call instances are retained independently even when their index collides.
func TestToolCallAccumulatorOpensNewCallOnIDChange(t *testing.T) {
	t.Parallel()

	accumulator := util.NewToolCallAccumulator()
	accumulator.Add(0, "call_a", "first", `{"a":1}`)
	first := accumulator.CompleteAll()
	testutil.Equal(t, len(first), 1)
	testutil.Equal(t, first[0].ID, "call_a")

	// The same index is reused for a different call.
	accumulator.Add(0, "call_b", "second", `{"b":2}`)
	second := accumulator.CompleteAll()
	testutil.Falsef(t, len(second) != 1 || second[0].ID != "call_b" || second[0].Name != "second", "second flush = %+v, want the reused-index call", second)

	// A repeated id must not emit again.
	accumulator.Add(0, "call_b", "", ``)
	got := accumulator.CompleteAll()
	testutil.Falsef(t, len(got) != 0, "third flush = %+v, want nothing", got)
}

// TestToolCallAccumulatorPreservesCallsAtReusedIndex proves an index collision
// does not silently discard one parallel call or merge their arguments.
func TestToolCallAccumulatorPreservesCallsAtReusedIndex(t *testing.T) {
	t.Parallel()

	accumulator := util.NewToolCallAccumulator()
	accumulator.Add(1, "call_c", "third", `{"c":3}`)
	accumulator.Add(1, "call_d", "fourth", `{"d":4}`)
	flushed := accumulator.CompleteAll()
	testutil.Falsef(t, len(flushed) != 2 || flushed[0].ID != "call_c" || flushed[1].ID != "call_d", "flush = %+v, want both calls in arrival order", flushed)
	got := accumulator.CompleteAll()
	testutil.Falsef(t, len(got) != 0, "re-flush = %+v, want nothing", got)
}

// TestConsumeStreamRejectsEventError proves an explicit error event terminates
// with an error rather than an empty success.
func TestConsumeStreamRejectsEventError(t *testing.T) {
	t.Parallel()

	body := "event: error\ndata: {}\n\n"
	_, _, err := collectStream(t, body)
	testutil.Error(t, err)
}

// TestReadSSEHandlesMultilineData proves a frame split across several data lines
// is joined with newlines, which is how long JSON payloads arrive.
func TestReadSSEHandlesMultilineData(t *testing.T) {
	t.Parallel()

	var frames []sseFrame
	err := readSSE(strings.NewReader("event: message\ndata: line1\ndata: line2\n\n"), func(frame sseFrame) bool {
		frames = append(frames, frame)
		return true
	})
	testutil.NoError(t, err, "readSSE() error = %v")
	testutil.Equal(t, len(frames), 1)
	testutil.Equal(t, frames[0].event, "message")
	testutil.Equal(t, frames[0].data, "line1\nline2")
}

// TestReadSSEPropagatesReadErrors proves a transport failure is visible.
func TestReadSSEPropagatesReadErrors(t *testing.T) {
	t.Parallel()

	err := readSSE(io.MultiReader(strings.NewReader("data: x\n"), errReader{}), func(sseFrame) bool { return true })
	testutil.False(t, err == nil, "readSSE() error = nil for a failing reader")
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

// TestFinishReasonMapping pins the stop-reason translation.
func TestFinishReasonMapping(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"stop", "end_turn"},
		{"tool_calls", "tool_use"},
		{"length", "max_tokens"},
		{"content_filter", "refusal"},
		{"", "end_turn"},
	}
	for _, tc := range cases {
		testutil.CheckEqual(t, (streamResult{FinishReasonValue: tc.in}).FinishReason(), tc.want)
	}
	testutil.CheckEqual(t, (streamResult{ToolCallCount: 1}).FinishReason(), "tool_use")
}

// TestEntitlementRefusalDoesNotRetireTheAccount is the regression test for the
// misdiagnosis observed on a live account.
//
// The gateway reports a missing subscription with HTTP 200, a business status of
// 403 and a body naming its pricing page. That message reached the shared account
// classifier, which read "403" as a dead credential and set the account's status
// to "403" — the console showed 「禁止访问」 for an account whose credential was
// perfectly valid, and the pool alarm fired for a channel that was not broken.
//
// The fixture is built with encoding/json rather than hand-escaped, because the
// escaping is exactly what a hand-written fixture gets wrong.
func TestEntitlementRefusalDoesNotRetireTheAccount(t *testing.T) {
	t.Parallel()

	// The inner payload the gateway nests as a JSON string.
	inner := `{"code":"112","message":"{\"pricingUrl\":\"https://qoder.com/pricing?client=qoder\"}"}`
	envelopeBody, err := json.Marshal(map[string]any{
		"headers":         map[string][]string{"Content-Type": {"application/json"}},
		"body":            inner,
		"statusCodeValue": 403,
		"statusCode":      "FORBIDDEN",
	})
	testutil.NoError(t, err, "marshal fixture: %v")
	stream := "data:" + string(envelopeBody) + "\n\n"

	_, _, streamErr := collectStream(t, stream)
	testutil.False(t, streamErr == nil, "consumeStream() error = nil, want an entitlement refusal")
	testutil.Falsef(t, !errors.Is(streamErr, ErrNoEntitlement), "error = %v, want ErrNoEntitlement", streamErr)

	text := streamErr.Error()

	// The shared classifier's exact triggers. Any of these would set the
	// account's status and disable it.
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"status=403", "403", "forbidden", "unauthorized"} {
		testutil.CheckNotContain(t, lower, forbidden)
	}

	// And the operator must still learn what is actually wrong. The code marker is
	// asserted too, so this test cannot pass through the generic error path: it
	// must be the entitlement branch that produced the message.
	testutil.MustContain(t, text, "code=112")
	testutil.CheckContain(t, text, "pricing")
	testutil.CheckFalsef(t, !strings.Contains(text, "plan") && !strings.Contains(text, "subscription"), "error text %q does not say what to do", text)
}
