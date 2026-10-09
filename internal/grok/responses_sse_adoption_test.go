package grok

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/responses"
	"orchids-api/internal/testutil"
	"strings"
	"testing"
	"testing/iotest"
)

func TestResponsesNativeOutcomeStatusAndEOF(t *testing.T) {
	for _, test := range []struct {
		status, finish string
		fail           bool
	}{
		{"completed", "stop", false}, {"incomplete", "length", false},
		{"failed", "error", true}, {"cancelled", "error", true},
		{"in_progress", "error", true}, {"queued", "error", true},
	} {
		t.Run(test.status, func(t *testing.T) {
			response := map[string]interface{}{"status": test.status}
			raw := strings.TrimRight(parityFrame("response.completed", map[string]interface{}{"response": response}), "\n")
			// Byte-sized reads and no final LF exercise the shared production decoder.
			_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), iotest.OneByteReader(strings.NewReader(raw)), "text/event-stream", "grok-4.6")
			testutil.Falsef(t, result.Finish != test.finish || (result.Err != nil) != test.fail, "%+v", result)
		})
	}
	for _, status := range []string{"queued", "in_progress"} {
		_, _, result := copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader("{\"status\":\""+status+"\"}"), "application/json", "grok-4.6")
		testutil.Equal(t, result.Finish, status)
		testutil.Equal(t, result.Err, nil)
	}
}

type compatShortWriter struct{ http.ResponseWriter }

func (w compatShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestResponsesNativeOutcomeShortWriteAndStickyFailure(t *testing.T) {
	_, _, result := copyNativeCLIResponseAndCaptureModel(compatShortWriter{httptest.NewRecorder()}, strings.NewReader(parityTerminal("response.completed")), "text/event-stream", "grok-4.6")
	testutil.Fail(t, !errors.Is(result.Err, io.ErrShortWrite) || result.Finish != "error", result)
	stream := parityFrame("response.failed", map[string]interface{}{"response": map[string]interface{}{"error": map[string]interface{}{"message": "failed first"}}}) + parityTerminal("response.completed")
	_, _, result = copyNativeCLIResponseAndCaptureModel(httptest.NewRecorder(), strings.NewReader(stream), "text/event-stream", "grok-4.6")
	testutil.Fail(t, result.Err == nil || result.Finish != "error", result)
}

func TestResponsesNativeAuditBoundsWholeMultilineFrame(t *testing.T) {
	line := "data: " + strings.Repeat("a", 64<<10) + "\n"
	recorder := httptest.NewRecorder()
	_, capture, result := copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader(strings.Repeat(line, 130)), "text/event-stream", "grok-4.6")
	if result.Err == nil || len(capture) > responses.MaxEventBytes || !strings.Contains(recorder.Body.String(), "response.failed") {
		t.Fatal("multiline audit accumulation is unbounded", result)
	}
}

type compatUnexpectedReader struct{ reads int }

func (r *compatUnexpectedReader) Read([]byte) (int, error) {
	r.reads++
	return 0, errors.New("read after logical terminal")
}

func TestResponsesNativeSSEFramingAndLogicalTerminal(t *testing.T) {
	input := "\uFEFF: keepalive\r\nid: event_a\r\nretry: 1000\r\nevent: response.completed\r\ndata: {\"response\":\r\ndata: {\"id\":\"resp_a\",\"status\":\"completed\"}}\r\n\r\n"
	tail := &compatUnexpectedReader{}
	recorder := httptest.NewRecorder()
	id, capture, result := copyNativeCLIResponseAndCaptureModel(recorder, io.MultiReader(strings.NewReader(input), tail), "text/event-stream", "grok-4.6")
	testutil.Fail(t, id != "resp_a" || tail.reads != 0 || result.Err != nil || result.Finish != "stop", id, tail.reads, result)
	for _, want := range []string{": keepalive", "id: event_a", "retry: 1000"} {
		if !strings.Contains(recorder.Body.String(), want) || !bytes.Contains(capture, []byte(want)) {
			t.Fatal("SSE metadata lost", want)
		}
	}
	// The relay passes the native Responses stream through: no added [DONE], and the
	// upstream's own framing (CRLF, multi-line data) is what the client receives.
	testutil.MustNotContain(t, recorder.Body.String(), "data: [DONE]")

	// A frame that already carries every field the strict clients need is relayed
	// byte-for-byte, CRLF included: supplementation is what makes a frame differ,
	// never the relay itself.
	complete := "event: response.completed\r\ndata: { \"type\":\"response.completed\", \"id\":\"resp_ok\", \"response\":{\"id\":\"resp_ok\",\"object\":\"response\",\"created_at\":42,\"model\":\"grok-4.6\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\",\"annotations\":[]}]}]}}\r\n\r\n"
	verbatim := httptest.NewRecorder()
	_, _, verbatimResult := copyNativeCLIResponseAndCaptureModel(verbatim, strings.NewReader(complete), "text/event-stream", "grok-4.6")
	testutil.Fail(t, verbatimResult.Err != nil, verbatimResult.Err)
	testutil.Equal(t, verbatim.Body.String(), complete)
}

// An upstream [DONE] without a terminal response event is still a failure the
// client has to see; the relay reports it instead of inventing a completion.
func TestResponsesNativeDoneWithoutTerminalSynthesizesFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	_, _, _ = copyNativeCLIResponseAndCaptureModel(recorder, strings.NewReader("data: [DONE]\n\n"), "text/event-stream", "grok-4.6")
	output := recorder.Body.String()
	testutil.MustContain(t, output, "event: response.failed")
	testutil.MustNotContain(t, output, "data: [DONE]")
	testutil.MustContain(t, output, "upstream_terminal_missing")
}
