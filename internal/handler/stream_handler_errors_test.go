package handler

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"orchids-api/internal/adapter"
	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

type terminalFlushFailureWriter struct {
	*httptest.ResponseRecorder
	failure error
	writes  int
}

func (w *terminalFlushFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.ResponseRecorder.Write(p)
}
func (w *terminalFlushFailureWriter) FlushError() error { return w.failure }

func TestStreamFlushFailureCancelsAndDoesNotEmitSuccessfulTerminal(t *testing.T) {
	failure := errors.New("flush failed")
	for _, format := range []adapter.ResponseFormat{adapter.FormatAnthropic, adapter.FormatOpenAI} {
		target := &terminalFlushFailureWriter{ResponseRecorder: httptest.NewRecorder(), failure: failure}
		logger := debug.New(false, false)
		defer logger.Close()
		sh := newStreamHandler(&config.Config{}, target, logger, true, format)
		defer sh.release()
		canceled := false
		sh.cancelUpstream = func() { canceled = true }
		sh.handleMessage(upstream.SSEMessage{Type: "model.text-delta", Event: map[string]any{"delta": "hello"}})
		sh.finishResponse("end_turn")
		if !sh.requestFailed || sh.finalStopReason != "write_error" || !canceled || target.writes != 1 {
			t.Fatalf("failed=%v reason=%s canceled=%v writes=%d", sh.requestFailed, sh.finalStopReason, canceled, target.writes)
		}
		testutil.MustNotContainAny(t, target.Body.String(), "[DONE]", "message_stop")
	}
}

func TestInjectNoAvailableAccountError_RateLimitAnswers429(t *testing.T) {
	rec := httptest.NewRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), false, adapter.FormatAnthropic)

	sh.InjectNoAvailableAccountError(
		`upstream API error: status=429, body={"code":"rate-limited","message":"You have hit the rate limit."}`,
		errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are rate-limited or cooling down)"),
	)

	// A non-streaming request has committed nothing yet, so the failure is a real
	// error response. It used to be a 200 whose assistant content was the error
	// text, which a client cannot tell from an answer.
	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	body := rec.Body.String()
	testutil.MustContainAll(t, body, `"type":"error"`, "rate-limited")
	testutil.MustNotContain(t, body, "Please check account statuses in Admin UI")
	// The selector error is a diagnostic: it belongs in the log, not in a body a
	// client may show to a user.
	testutil.MustNotContain(t, body, "no enabled accounts available for channel")
}

// TestInjectNoAvailableAccountError_CreditExhaustionIsChannelNeutral pins that the
// spent-allowance message names no channel: whichever provider ran out says the
// same thing, and the upstream's own text never reaches the client.
func TestInjectNoAvailableAccountError_CreditExhaustionIsChannelNeutral(t *testing.T) {
	rec := httptest.NewRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), false, adapter.FormatAnthropic)

	sh.InjectNoAvailableAccountError(
		`workbuddy API error: status=429, message={"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs"}}}`,
		nil,
	)

	testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
	body := rec.Body.String()
	testutil.MustNotContain(t, body, "workbuddy API error")
	testutil.MustContainAll(t, body, `"type":"error"`, "exhausted its allowance")
}

// TestInjectNoAvailableAccountError_StreamingReportsInBandError is the other half
// of the contract: once the stream has actually started, its status is already
// 200 and can never be revisited.
//
// It must not pretend to be an answer either. The report is the protocol's error
// event, and the stream ends there rather than with a normal stop. A stream that
// has sent nothing yet is deliberately NOT in this case -- see
// TestStreamError_OpenAIFormatRespectsWhetherTheStreamStarted, which pins that an
// uncommitted response still answers with a real status.
func TestInjectNoAvailableAccountError_StreamingReportsInBandError(t *testing.T) {
	rec := httptest.NewRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatAnthropic)
	// Commit the response the way a started stream does: the opening frame is
	// written lazily now, so a stream only becomes unrevistable once it has begun.
	// Opening without content keeps this test about the in-band report rather than
	// about the answer text.
	sh.mu.Lock()
	sh.writeMessageStartLocked("workbuddy-model", 12, 0)
	sh.mu.Unlock()

	sh.InjectNoAvailableAccountError(
		`upstream API error: status=429, body={"code":"rate-limited"}`,
		errors.New("no enabled accounts available for channel: workbuddy (all matching accounts are rate-limited or cooling down)"),
	)

	testutil.Equal(t, rec.Code, http.StatusOK)
	body := rec.Body.String()
	testutil.MustContainAll(t, body, "event: error", `"type":"error"`)
	// The failure must not be dressed as assistant text.
	testutil.MustNotContain(t, body, "content_block_delta")
	testutil.MustNotContain(t, body, "no enabled accounts available for channel")
}

// TestStreamError_OpenAIFormatRespectsWhetherTheStreamStarted pins the two
// answers the streaming path owes a caller, and keeps them consistent with the
// initial-selection entrance that reports the same condition.
//
// Before any content the response is still uncommitted, so the failure is an
// HTTP status: the client sees a retryable 429 rather than a truncated stream.
// Once content has been sent the status is fixed, so the same failure has to
// terminate the stream in band.
func TestStreamError_OpenAIFormatRespectsWhetherTheStreamStarted(t *testing.T) {
	t.Run("nothing sent yet answers with a status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatOpenAI)

		sh.InjectNoAvailableAccountError(`upstream API error: status=429`, errors.New("no enabled accounts available"))

		testutil.Equal(t, rec.Code, http.StatusTooManyRequests)
		body := rec.Body.String()
		testutil.MustContainAll(t, body, `"error":{`, "rate-limited")
		testutil.MustNotContainAny(t, body, "data:", "[DONE]")
	})

	t.Run("after content it terminates the stream in band", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), true, adapter.FormatOpenAI)
		sh.handleMessage(upstream.SSEMessage{
			Type:  "model.text-delta",
			Event: map[string]any{"delta": "partial"},
		})

		sh.InjectNoAvailableAccountError(`upstream API error: status=429`, errors.New("no enabled accounts available"))

		body := rec.Body.String()
		testutil.MustContainAll(t, body, `"error":{`, "rate-limited")
		testutil.MustContain(t, body, "[DONE]")
	})
}

func TestStreamHandler_TerminalWriteFailureOverridesClaimedSuccess(t *testing.T) {
	writer := &failingResponseWriter{err: errors.New("client connection closed")}
	logger := debug.New(false, false)
	defer logger.Close()
	sh := newStreamHandler(&config.Config{}, writer, logger, true, adapter.FormatAnthropic)
	defer sh.release()

	sh.finishResponse("end_turn")

	returned, failed := sh.terminalState()
	testutil.Falsef(t, !returned || !failed, "terminal state = returned:%v failed:%v, want terminal failure", returned, failed)
	testutil.Equal(t, sh.finalStopReason, "write_error")
}

func TestStreamHandler_TerminalStateAndKeepAliveAreRaceSafe(t *testing.T) {
	sh, _ := newStreamTestHandler(t, true, adapter.FormatAnthropic)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 100 {
			sh.writeKeepAlive()
		}
	}()
	go func() {
		defer wg.Done()
		for range 100 {
			_, _ = sh.terminalState()
		}
		sh.finishResponse("end_turn")
	}()
	wg.Wait()

	returned, _ := sh.terminalState()
	testutil.False(t, !returned, "handler did not reach terminal state")
}

// TestReportRequestFailure_ClientRejectionAnswers400 pins the branch the
// non-retryable upstream failure takes: a rejection of the request itself is 400,
// and the client still gets the operator-facing text.
//
// This path used to answer 200 with the message as assistant content, which is
// indistinguishable from an answer.
func TestReportRequestFailure_ClientRejectionAnswers400(t *testing.T) {
	rec := httptest.NewRecorder()
	sh := newStreamHandler(&config.Config{}, rec, debug.New(false, false), false, adapter.FormatAnthropic)

	sh.reportRequestFailure("probe", "client", "The upstream rejected the request parameters or model. Check the request and model selection.", 0)

	testutil.Equal(t, rec.Code, http.StatusBadRequest)
	body := rec.Body.String()
	testutil.MustContainAll(t, body, `"type":"error"`, "rejected the request parameters")
}
