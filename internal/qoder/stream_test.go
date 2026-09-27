package qoder

import (
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"orchids-api/internal/upstream"
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
	result, err := consumeStreamWithTools(strings.NewReader(body), false, func(msg upstream.SSEMessage) {
		events = append(events, msg)
	})
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
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}

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
	if text.String() != "Hello" {
		t.Fatalf("text = %q, want Hello", text.String())
	}
	if reasoning.String() != "think" {
		t.Fatalf("reasoning = %q, want think", reasoning.String())
	}
	if got := result.FinishReason(); got != "end_turn" {
		t.Fatalf("FinishReason() = %q, want end_turn", got)
	}
	if !result.SawMeaningfulEvent {
		t.Fatal("SawMeaningfulEvent = false")
	}
}

func TestConsumeStreamClassifiesTextRateLimit(t *testing.T) {
	t.Parallel()
	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"The available upstream accounts are rate-limited. Retry after the cooldown. Request ID: abc"},"finish_reason":"stop"}]}`) +
		"event:finish\ndata: {}\n\n"
	events, _, err := collectStream(t, body)
	if !errors.Is(err, ErrModelRateLimited) {
		t.Fatalf("error = %v, want ErrModelRateLimited", err)
	}
	if len(events) != 0 {
		t.Fatalf("rate-limit text must not be forwarded as output: %+v", events)
	}
}

// TestConsumeStreamRequiresTerminator proves a premature EOF is reported as a
// truncation instead of as a successful short answer. Silently accepting it
// would hand the client a cut-off response with no error.
func TestConsumeStreamRequiresTerminator(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)
	_, result, err := collectStream(t, body)
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("error = %v, want ErrStreamTruncated", err)
	}
	if !result.SawMeaningfulEvent {
		t.Fatal("SawMeaningfulEvent = false, want the partial content to be recorded")
	}
}

// A done marker without Qoder's final event must not hide a truncated tail.
func TestConsumeStreamBareDoneRequiresFinish(t *testing.T) {
	t.Parallel()
	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"ok"}}]}`) + "data: [DONE]\n\n"
	if _, _, err := collectStream(t, body); !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("error = %v, want ErrStreamTruncated", err)
	}
}

func TestConsumeStreamEnvelopeDoneRequiresFinish(t *testing.T) {
	t.Parallel()
	if _, _, err := collectStream(t, envelope(`[DONE]`)); !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("error = %v, want ErrStreamTruncated", err)
	}
}

// TestConsumeStreamReportsErrorEnvelope proves an upstream failure is surfaced
// rather than being swallowed as an empty stream.
func TestConsumeStreamReportsErrorEnvelope(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":500,"body":"{\"message\":\"model overloaded\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if err == nil {
		t.Fatal("consumeStream() error = nil for an error envelope")
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Fatalf("error = %v, want the upstream message", err)
	}
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
	if !errors.Is(err, ErrModelRateLimited) {
		t.Fatalf("error = %v, want ErrModelRateLimited", err)
	}
	if len(events) != 0 {
		t.Fatalf("rate-limit sentinel leaked as assistant output: %+v", events)
	}
}

func TestConsumeStreamClassifiesBusyCode(t *testing.T) {
	t.Parallel()

	for _, bodyJSON := range []string{
		`{"code":"10605","message":"queue full"}`,
		`{"code":10605,"message":"{\"isQueued\":true,\"retryAfterSeconds\":29,\"serviceAvailable\":false,\"waitTime\":29}"}`,
	} {
		body := "data: " + `{"statusCodeValue":401,"body":` + strconv.Quote(bodyJSON) + `}` + "\n\n"
		_, _, err := collectStream(t, body)
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("error = %v, want ErrBusy for %s", err, bodyJSON)
		}
		if errors.Is(err, errUpstreamUnauthorized) {
			t.Fatalf("busy refusal was misclassified as unauthorized: %v", err)
		}
		var attemptErr *attemptStreamError
		if !errors.As(err, &attemptErr) || !attemptErr.busy || !attemptErr.retryable {
			t.Fatalf("busy refusal lacks typed retry metadata: %#v", err)
		}
		if strings.Contains(bodyJSON, "retryAfterSeconds") && attemptErr.RetryAfter() != 29*time.Second {
			t.Fatalf("RetryAfter=%v want 29s", attemptErr.RetryAfter())
		}
	}
}

func TestConsumeStreamPreservesEnvelopeStatusForClassification(t *testing.T) {
	t.Parallel()
	body := "data: " + `{"statusCodeValue":400,"body":"{\"message\":\"invalid tool schema\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if err == nil || !strings.Contains(err.Error(), "status=400") {
		t.Fatalf("error = %v, want explicit status=400", err)
	}
}

func TestConsumeStreamClassifiesAgentLimitWithoutClaimingAccountQuota(t *testing.T) {
	t.Parallel()
	const resetMillis = int64(1790538433100)
	body := "data: " + `{"statusCodeValue":401,"body":"{\"message\":\"{\\\"agentLimitResetTime\\\":1790538433100}\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	var agentErr *agentLimitError
	if !errors.As(err, &agentErr) {
		t.Fatalf("error = %v, want agentLimitError", err)
	}
	if want := time.UnixMilli(resetMillis); !agentErr.resetAt.Equal(want) {
		t.Fatalf("reset=%v want %v", agentErr.resetAt, want)
	}
	if strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("agent-scoped refusal was presented as account quota exhaustion: %v", err)
	}
}

func TestConsumeStreamDoesNotRetryDuplicateRequest(t *testing.T) {
	t.Parallel()
	body := "data: " + `{"statusCodeValue":401,"body":"{\"message\":\"Duplicate request\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if err == nil || !strings.Contains(err.Error(), "duplicate request") || errors.Is(err, errUpstreamUnauthorized) {
		t.Fatalf("error = %v, want non-auth duplicate request", err)
	}
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
	if err == nil {
		t.Fatal("consumeStream() error = nil for an entitlement refusal")
	}
	if !errors.Is(err, ErrNoEntitlement) {
		t.Fatalf("error = %v, want ErrNoEntitlement", err)
	}
	// The account classifier reads any "status=403" as a dead credential, so the
	// error text must not carry the upstream status.
	if strings.Contains(err.Error(), "status=403") {
		t.Fatalf("error text = %q, want no upstream status (it would retire the account)", err)
	}
	if strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error text = %q, want no forbidden wording", err)
	}
	// The reason must reach the operator.
	if !strings.Contains(err.Error(), "pricing") && !strings.Contains(err.Error(), "plan") {
		t.Fatalf("error text = %q, want the entitlement reason", err)
	}
}

// TestConsumeStreamClassifiesUnauthorizedEnvelope proves a genuine auth failure
// is distinguished from a busy verdict.
func TestConsumeStreamClassifiesUnauthorizedEnvelope(t *testing.T) {
	t.Parallel()

	body := "data: " + `{"statusCodeValue":403,"body":"{\"message\":\"login expired\"}"}` + "\n\n"
	_, _, err := collectStream(t, body)
	if !errors.Is(err, errUpstreamUnauthorized) {
		t.Fatalf("error = %v, want errUpstreamUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "status=403") && !strings.Contains(err.Error(), "login expired") {
		t.Fatalf("error = %v, want the upstream reason", err)
	}
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
	if err == nil || !strings.Contains(err.Error(), "protocol error") {
		t.Fatalf("consumeStream() error = %v, want protocol error", err)
	}
}

// TestConsumeStreamUsageSurvivesASharedFrame proves usage is captured even when
// it rides along with the final content frame, which is how the gateway reports
// the last chunk.
func TestConsumeStreamUsageSurvivesASharedFrame(t *testing.T) {
	t.Parallel()

	body := envelope(`{"id":"1","choices":[{"index":0,"delta":{"content":"final"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3,"cacheable_tokens":5},"completion_tokens_details":{"reasoning_tokens":2},"credits":0.25,"original_credits":0.5}}`) +
		"event:finish\ndata: {}\n\n"

	events, result, err := collectStream(t, body)
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
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
	if !sawText {
		t.Fatal("the content in a shared usage frame was dropped")
	}
	if !sawUsage {
		t.Fatal("usage was not emitted")
	}
	if got := result.Usage["inputTokens"]; got != 11 {
		t.Fatalf("inputTokens = %v, want 11", got)
	}
	if got := result.Usage["outputTokens"]; got != 7 {
		t.Fatalf("outputTokens = %v, want 7", got)
	}
	if got := result.Usage["cacheReadTokens"]; got != 3 {
		t.Fatalf("cacheReadTokens = %v, want 3", got)
	}
	if got := result.Usage["cacheWriteTokens"]; got != 5 {
		t.Fatalf("cacheWriteTokens = %v, want 5", got)
	}
	if got := result.Usage["credits"]; got != 0.25 {
		t.Fatalf("credits = %v, want 0.25", got)
	}
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
	if err != nil {
		t.Fatalf("consumeStream() error = %v", err)
	}
	var call map[string]interface{}
	for _, event := range events {
		if event.Type == "model.tool-call" {
			call = event.Event
		}
	}
	if call == nil {
		t.Fatalf("no tool call was emitted; events = %+v", events)
	}
	if call["toolName"] != "search" {
		t.Fatalf("toolName = %v, want search", call["toolName"])
	}
	if call["input"] != `{"q":"cats"}` {
		t.Fatalf("input = %v, want the reassembled arguments", call["input"])
	}
	if got := result.FinishReason(); got != "tool_use" {
		t.Fatalf("FinishReason() = %q, want tool_use", got)
	}
	if result.ToolCallCount != 1 {
		t.Fatalf("ToolCallCount = %d, want 1", result.ToolCallCount)
	}
}

// TestToolCallAccumulatorOpensNewCallOnIDChange covers parallel calls, which the
// upstream sometimes reports by reusing one index and changing only the id.
//
// Call instances are retained independently even when their index collides.
func TestToolCallAccumulatorOpensNewCallOnIDChange(t *testing.T) {
	t.Parallel()

	accumulator := newToolCallAccumulator()
	accumulator.add(0, "call_a", "first", `{"a":1}`)
	first := accumulator.completeAll()
	if len(first) != 1 || first[0].ID != "call_a" {
		t.Fatalf("first flush = %+v, want the first call", first)
	}

	// The same index is reused for a different call.
	accumulator.add(0, "call_b", "second", `{"b":2}`)
	second := accumulator.completeAll()
	if len(second) != 1 || second[0].ID != "call_b" || second[0].Name != "second" {
		t.Fatalf("second flush = %+v, want the reused-index call", second)
	}

	// A repeated id must not emit again.
	accumulator.add(0, "call_b", "", ``)
	if got := accumulator.completeAll(); len(got) != 0 {
		t.Fatalf("third flush = %+v, want nothing", got)
	}
}

// TestToolCallAccumulatorPreservesCallsAtReusedIndex proves an index collision
// does not silently discard one parallel call or merge their arguments.
func TestToolCallAccumulatorPreservesCallsAtReusedIndex(t *testing.T) {
	t.Parallel()

	accumulator := newToolCallAccumulator()
	accumulator.add(1, "call_c", "third", `{"c":3}`)
	accumulator.add(1, "call_d", "fourth", `{"d":4}`)
	flushed := accumulator.completeAll()
	if len(flushed) != 2 || flushed[0].ID != "call_c" || flushed[1].ID != "call_d" {
		t.Fatalf("flush = %+v, want both calls in arrival order", flushed)
	}
	if got := accumulator.completeAll(); len(got) != 0 {
		t.Fatalf("re-flush = %+v, want nothing", got)
	}
}

// TestConsumeStreamRejectsEventError proves an explicit error event terminates
// with an error rather than an empty success.
func TestConsumeStreamRejectsEventError(t *testing.T) {
	t.Parallel()

	body := "event: error\ndata: {}\n\n"
	if _, _, err := collectStream(t, body); err == nil {
		t.Fatal("consumeStream() error = nil for an error event")
	}
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
	if err != nil {
		t.Fatalf("readSSE() error = %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	if frames[0].event != "message" || frames[0].data != "line1\nline2" {
		t.Fatalf("frame = %+v, want the joined data", frames[0])
	}
}

// TestReadSSEPropagatesReadErrors proves a transport failure is visible.
func TestReadSSEPropagatesReadErrors(t *testing.T) {
	t.Parallel()

	err := readSSE(io.MultiReader(strings.NewReader("data: x\n"), errReader{}), func(sseFrame) bool { return true })
	if err == nil {
		t.Fatal("readSSE() error = nil for a failing reader")
	}
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
		if got := (streamResult{FinishReasonValue: tc.in}).FinishReason(); got != tc.want {
			t.Errorf("FinishReason(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := (streamResult{ToolCallCount: 1}).FinishReason(); got != "tool_use" {
		t.Errorf("FinishReason with a tool call = %q, want tool_use", got)
	}
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
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	stream := "data:" + string(envelopeBody) + "\n\n"

	_, _, streamErr := collectStream(t, stream)
	if streamErr == nil {
		t.Fatal("consumeStream() error = nil, want an entitlement refusal")
	}
	if !errors.Is(streamErr, ErrNoEntitlement) {
		t.Fatalf("error = %v, want ErrNoEntitlement", streamErr)
	}

	text := streamErr.Error()

	// The shared classifier's exact triggers. Any of these would set the
	// account's status and disable it.
	lower := strings.ToLower(text)
	for _, forbidden := range []string{"status=403", "403", "forbidden", "unauthorized"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("error text %q contains %q, which the account classifier reads as a dead credential", text, forbidden)
		}
	}

	// And the operator must still learn what is actually wrong. The code marker is
	// asserted too, so this test cannot pass through the generic error path: it
	// must be the entitlement branch that produced the message.
	if !strings.Contains(text, "code=112") {
		t.Fatalf("error text %q was not produced by the entitlement branch", text)
	}
	if !strings.Contains(text, "pricing") {
		t.Errorf("error text %q does not name the pricing page", text)
	}
	if !strings.Contains(text, "plan") && !strings.Contains(text, "subscription") {
		t.Errorf("error text %q does not say what to do", text)
	}
}
