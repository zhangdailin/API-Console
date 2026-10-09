package httpserver

import (
	"bytes"
	"net/http"
)

type CaptureResponseWriter struct {
	header http.Header
	Body   bytes.Buffer
	Code   int
}

func NewCaptureResponseWriter() *CaptureResponseWriter {
	return &CaptureResponseWriter{header: make(http.Header), Code: http.StatusOK}
}

func (w *CaptureResponseWriter) Header() http.Header { return w.header }

func (w *CaptureResponseWriter) WriteHeader(Code int) {
	if Code != 0 {
		w.Code = Code
	}
}

func (w *CaptureResponseWriter) Write(p []byte) (int, error) { return w.Body.Write(p) }

func (w *CaptureResponseWriter) Flush() {}
