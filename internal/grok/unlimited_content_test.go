package grok

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type whitespaceReader struct{}

func (whitespaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

type discardResponseWriter struct{ header http.Header }

func (w discardResponseWriter) Header() http.Header       { return w.header }
func (discardResponseWriter) WriteHeader(int)             {}
func (discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestNativeJSONBeyondFormerLimit(t *testing.T) {
	const padding = 128 << 20
	prefix := `{"id":"large","status":"completed"}`
	source := io.MultiReader(strings.NewReader(prefix), io.LimitReader(whitespaceReader{}, padding))
	id, capture, result := copyNativeCLIResponseAndCaptureModel(discardResponseWriter{make(http.Header)}, source, "application/json", "grok-4.7")
	if result.Err != nil || id != "large" || len(capture) != len(prefix)+padding {
		t.Fatal("large JSON lost", id, len(capture), result.Err)
	}
}

func TestSemanticActivityBeyondFormerEventLimit(t *testing.T) {
	d := &buildSSEActivityDetector{}
	payload := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", 9<<20) + "\"}\n\n"
	active := false
	for start := 0; start < len(payload); start += 32768 {
		active = d.Observe([]byte(payload[start:min(start+32768, len(payload))])) || active
	}
	if !active {
		t.Fatal("large generated event treated as idle")
	}
}
