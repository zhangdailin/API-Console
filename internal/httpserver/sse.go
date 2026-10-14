package httpserver

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/debug"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/util"
)

var (
	sseEventPrefixBytes = []byte("event: ")
	sseDataPrefixBytes  = []byte("data: ")
	sseNewlineBytes     = []byte("\n")
	sseFrameSuffixBytes = []byte("\n\n")
)

// RequireMethod writes the standard 405 response and returns false when the
// request method does not match. Handlers use it as:
//
//	if !httpserver.RequireMethod(w, r, http.MethodGet) {
//		return
//	}
//
// The answer is the shared OpenAI error envelope, which is what an inference
// client can parse. Admin endpoints that answer plain text use
// RequirePlainMethod instead.
func RequireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		WriteError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

// RequirePlainMethod is RequireMethod for endpoints whose 405 body is the
// plain-text http.Error form, such as the admin console routes.
func RequirePlainMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	WritePlainMethodNotAllowed(w)
	return false
}

// WritePlainMethodNotAllowed answers the shared plain-text 405 body for the
// handlers that dispatch on the method themselves.
func WritePlainMethodNotAllowed(w http.ResponseWriter) {
	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

// RequireAPIKeyModel rejects a model the caller's key is not allowed to use.
func RequireAPIKeyModel(w http.ResponseWriter, r *http.Request, model string) bool {
	if middleware.APIKeyAllowsModel(r.Context(), model) {
		return true
	}
	// The whole envelope shape is shared with every other error, so a client
	// can parse one object type: message, type, code and param.
	WriteErrorCode(w, http.StatusForbidden, "model_not_allowed",
		"API key is not allowed to use model "+strings.TrimSpace(model))
	return false
}

// StreamResponseHeaders writes the standard SSE headers and returns the
// response flusher (possibly nil).
func StreamResponseHeaders(w http.ResponseWriter) http.Flusher {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// Without this a reverse proxy (nginx defaults to proxy_buffering on) holds
	// the frames until the response ends, which silently defeats streaming.
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Del("Connection")
	flusher, _ := w.(http.Flusher)
	return flusher
}

// responseWriteTimeout bounds downstream backpressure once the status line is
// committed.
const responseWriteTimeout = 30 * time.Second

// setResponseWriteDeadline bounds downstream backpressure when the writer's
// transport supports deadlines. In-memory/test writers legitimately do not.
func setResponseWriteDeadline(w http.ResponseWriter) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func writeAll(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return err
}

// DeadlineResponseWriter wraps a response writer so every write refreshes the
// write deadline. A slow or vanished client then fails the write instead of
// holding a connection open indefinitely.
type DeadlineResponseWriter struct{ http.ResponseWriter }

func (w DeadlineResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w DeadlineResponseWriter) Flush() { _ = w.FlushError() }

// FlushError bounds the actual transport flush and preserves its error.
func (w DeadlineResponseWriter) FlushError() error {
	if err := setResponseWriteDeadline(w.ResponseWriter); err != nil {
		return err
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Write refreshes the deadline and forwards the write.
func (w DeadlineResponseWriter) Write(p []byte) (int, error) {
	if err := setResponseWriteDeadline(w.ResponseWriter); err != nil {
		return 0, err
	}
	n, err := w.ResponseWriter.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

// WriteSSEBytes sends a raw SSE frame without flushing. Its error result may be
// ignored by legacy non-streaming helpers, while stream loops propagate it.
func WriteSSEBytes(w http.ResponseWriter, event string, data []byte) error {
	if err := setResponseWriteDeadline(w); err != nil {
		return err
	}
	return WriteSSEFrame(w, event, data)
}

// AppendSSEFrame appends one complete frame to caller-owned scratch space.
func AppendSSEFrame(frame []byte, event string, data []byte) []byte {
	if event != "" {
		frame = append(frame, sseEventPrefixBytes...)
		frame = append(frame, event...)
		frame = append(frame, sseNewlineBytes...)
	}
	frame = append(frame, sseDataPrefixBytes...)
	frame = append(frame, data...)
	frame = append(frame, sseFrameSuffixBytes...)
	return frame
}

// WriteSSEFrame also supports internal pipe writers and detects short writes.
func WriteSSEFrame(w io.Writer, event string, data []byte) error {
	size := len(sseDataPrefixBytes) + len(data) + len(sseFrameSuffixBytes)
	if event != "" {
		size += len(sseEventPrefixBytes) + len(event) + len(sseNewlineBytes)
	}
	return writeAll(w, AppendSSEFrame(make([]byte, 0, size), event, data))
}

// WriteSSEError sends an OpenAI-style SSE error event (no flush, no [DONE]).
//
// An SSE error is written after the 200 status line is already committed, so the
// HTTP status can no longer describe the outcome. The response writer is told
// about it instead, which is how the operations overview counts a stream that
// died after starting as a failure rather than a success.
func WriteSSEError(w http.ResponseWriter, message, errType, code string) {
	middleware.MarkStreamFailure(w)
	requestID := strings.TrimSpace(w.Header().Get(middleware.DiagnosticRequestIDHeader))
	if strings.TrimSpace(errType) == "" {
		errType = "server_error"
	}
	payload := map[string]any{
		// The top-level type is what OpenAI-compatible clients dispatch on; a
		// frame without it looks like an ordinary chunk to them.
		"type": "error",
		"error": map[string]any{
			"message":    apperrors.PublicMessage(message),
			"type":       strings.TrimSpace(errType),
			"code":       strings.TrimSpace(code),
			"request_id": requestID,
		},
	}
	WriteSSEBytes(w, "error", util.EncodeJSONBytes(payload))
}

// WriteSSEStreamError sends the named SSE error used by non-Chat protocols.
func WriteSSEStreamError(w http.ResponseWriter, flusher http.Flusher, logger *debug.Logger, msg string) {
	WriteSSEError(w, msg, "server_error", "stream_error")
	_ = WriteSSEBytes(w, "", []byte("[DONE]"))
	if logger != nil {
		logger.LogOutputSSE("error", msg)
		logger.LogOutputSSE("", "[DONE]")
	}
	if flusher != nil {
		flusher.Flush()
	}
}
