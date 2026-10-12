package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"orchids-api/internal/middleware"
)

// CheckedStreamWriter makes write failures visible to translators whose SSE
// helpers predate error returns. The caller then closes its pipe and cancels
// the upstream request instead of leaving a blocked producer behind.
type CheckedStreamWriter struct {
	Target io.Writer
	Err    error
	frame  []byte
}

func (w *CheckedStreamWriter) fail(err error) {
	if err == nil || w.Err != nil {
		return
	}
	w.Err = err
	if target, ok := w.Target.(http.ResponseWriter); ok {
		middleware.MarkStreamFailure(target)
	}
}

// Write forwards the write and remembers a failure or a short write.
func (w *CheckedStreamWriter) Write(data []byte) (int, error) {
	if w.Err != nil {
		return 0, w.Err
	}
	n, err := w.Target.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	w.fail(err)
	return n, err
}

// WriteSSE reuses frame storage until the synchronous Write has returned.
func (w *CheckedStreamWriter) WriteSSE(event string, data []byte) error {
	if w.Err != nil {
		return w.Err
	}
	return w.writeFrame(AppendSSEFrame(w.frame[:0], event, data))
}

// WriteSSEString avoids copying a rewritten SSE data line into a byte slice.
func (w *CheckedStreamWriter) WriteSSEString(event, data string) error {
	if w.Err != nil {
		return w.Err
	}
	frame := w.frame[:0]
	if event != "" {
		frame = append(frame, sseEventPrefixBytes...)
		frame = append(frame, event...)
		frame = append(frame, sseNewlineBytes...)
	}
	frame = append(frame, sseDataPrefixBytes...)
	frame = append(frame, data...)
	frame = append(frame, sseFrameSuffixBytes...)
	return w.writeFrame(frame)
}

func (w *CheckedStreamWriter) writeFrame(frame []byte) error {
	w.frame = frame
	_, err := w.Write(frame)
	if cap(w.frame) > 64<<10 {
		w.frame = nil
	}
	return err
}

// Flush flushes the target when it supports it.
func (w *CheckedStreamWriter) Flush() {
	if w.Err != nil {
		return
	}
	if target, ok := w.Target.(http.ResponseWriter); ok {
		err := http.NewResponseController(target).Flush()
		if !errors.Is(err, http.ErrNotSupported) {
			w.fail(err)
		}
		return
	}
	if f, ok := w.Target.(http.Flusher); ok {
		f.Flush()
	}
}

type streamingChatWriter struct {
	header          http.Header
	committedHeader http.Header
	pipe            *io.PipeWriter
	status          int
	once            sync.Once
	ready           chan struct{}
}

func newStreamingChatWriter(pipe *io.PipeWriter) *streamingChatWriter {
	return &streamingChatWriter{header: make(http.Header), pipe: pipe, ready: make(chan struct{})}
}

func (w *streamingChatWriter) Header() http.Header { return w.header }
func (w *streamingChatWriter) WriteHeader(status int) {
	w.once.Do(func() { w.status = status; w.committedHeader = w.header.Clone(); close(w.ready) })
}
func (w *streamingChatWriter) Write(data []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pipe.Write(data)
}
func (w *streamingChatWriter) Flush() { w.WriteHeader(http.StatusOK) }

// StreamThroughChat runs a chat handler with a streaming ResponseWriter and
// hands the status, the committed headers and the body reader to consume. The
// producer is cancelled when consume returns, so a client disconnect stops the
// upstream.
func StreamThroughChat(req *http.Request, chat http.HandlerFunc, consume func(int, http.Header, io.Reader)) {
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	// Cancellation must interrupt consume even when the producer is waiting
	// for upstream and is not writing to the pipe.
	stop := context.AfterFunc(ctx, func() {
		// Closing the write end interrupts both reads and writes while letting
		// the consumer observe the cancellation cause instead of ErrClosedPipe.
		_ = writer.CloseWithError(context.Cause(ctx))
	})
	defer stop()
	streamWriter := newStreamingChatWriter(writer)
	go func() {
		defer writer.Close()
		chat(streamWriter, req.Clone(ctx))
		streamWriter.WriteHeader(http.StatusOK)
	}()
	select {
	case <-streamWriter.ready:
		consume(streamWriter.status, streamWriter.committedHeader, reader)
	case <-ctx.Done():
	}
}
