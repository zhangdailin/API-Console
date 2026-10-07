package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestOutputEvidenceSurvivesFragmentedWritesAndSeparatesReasoning(t *testing.T) {
	w := NewTracedResponseWriter(httptest.NewRecorder())
	w.Header().Set("Content-Type", "text/event-stream")
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thought\"}}]}\n"))
	if !w.ContentWriteAt().IsZero() {
		t.Fatal("incomplete event counted")
	}
	w.Write([]byte("\n"))
	if w.ContentWriteAt().IsZero() || !w.visibleWriteAt.IsZero() {
		t.Fatal("reasoning confused with visible answer")
	}
	w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}},{\"finish_reason\":\"length\"}]}\n\n"))
	if w.visibleWriteAt.IsZero() || !w.tokenDetector.reasoning || !isTokenTruncation(w.tokenDetector.finish) {
		t.Fatal("lost late text or finish evidence")
	}
}

func TestOutputSignalsControlFramesAndToolOnly(t *testing.T) {
	var d tokenSSEDetector
	if d.observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n: ping\n\n")) {
		t.Fatal("control counted as generation")
	}
	if !d.observe([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":\"run\"}}]}}]}\n\n")) || !d.tool || d.visible {
		t.Fatal("tool-only evidence lost")
	}
	d.observe([]byte("event: response.incomplete\ndata: {\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"))
	if !isTokenTruncation(d.finish) {
		t.Fatal("Responses truncation lost")
	}
}

func TestToolIdentityAloneDoesNotCountAsOutput(t *testing.T) {
	s := streamOutputSignals("", []byte(`{"choices":[{"delta":{"tool_calls":[{"id":"call_1"}]}}]}`))
	if s.tool {
		t.Fatal("tool ID is not generated tool content")
	}
}
