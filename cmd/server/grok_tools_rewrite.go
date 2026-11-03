package main

import (
	"bufio"
	"bytes"
	"errors"
	"net"
	"net/http"
	"strings"
)

// The grok channel's handlers build resource links under the inference prefix
// (/grok/v1/files/..., /grok/v1/videos/{id}/content). Those routes require a
// managed API key, so a link handed to the admin tools page would answer 401
// to the operator's session. toolsLinkWriter rewrites the links in text
// responses of the session-authenticated surfaces to the tools projection,
// which serves the same resources behind the session.

var toolsLinkRewrites = []struct{ from, to []byte }{
	{[]byte("/grok/v1/files/"), []byte("/api/grok/tools/v1/files/")},
	{[]byte("/grok/v1/videos/"), []byte("/api/grok/tools/v1/videos/")},
}

type toolsLinkWriter struct {
	http.ResponseWriter
	decided bool
	rewrite bool
	pending []byte
}

func withToolsLinks(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writer := &toolsLinkWriter{ResponseWriter: w}
		next(writer, r)
		writer.flushPending()
	}
}

func (w *toolsLinkWriter) decide() {
	if w.decided {
		return
	}
	w.decided = true
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	w.rewrite = strings.Contains(contentType, "json") || strings.HasPrefix(contentType, "text/")
	if w.rewrite {
		// The rewrite changes the length.
		w.Header().Del("Content-Length")
	}
}

func (w *toolsLinkWriter) WriteHeader(status int) {
	w.decide()
	w.ResponseWriter.WriteHeader(status)
}

func (w *toolsLinkWriter) Write(p []byte) (int, error) {
	w.decide()
	if !w.rewrite {
		return w.ResponseWriter.Write(p)
	}
	data := append(w.pending, p...)
	for _, rule := range toolsLinkRewrites {
		data = bytes.ReplaceAll(data, rule.from, rule.to)
	}
	// A link split across two writes is completed by the next one: hold back
	// a suffix that could still become one.
	hold := 0
	for _, rule := range toolsLinkRewrites {
		for n := len(rule.from) - 1; n > hold; n-- {
			if n <= len(data) && bytes.HasSuffix(data, rule.from[:n]) {
				hold = n
				break
			}
		}
	}
	w.pending = append([]byte(nil), data[len(data)-hold:]...)
	if _, err := w.ResponseWriter.Write(data[:len(data)-hold]); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *toolsLinkWriter) flushPending() {
	if len(w.pending) > 0 {
		_, _ = w.ResponseWriter.Write(w.pending)
		w.pending = nil
	}
}

func (w *toolsLinkWriter) Flush() {
	w.flushPending()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Hijack keeps WebSocket upgrades (Imagine) working through the wrapper.
func (w *toolsLinkWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	return hijacker.Hijack()
}

func (w *toolsLinkWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
