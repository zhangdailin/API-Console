// Package middleware provides HTTP middleware.
package middleware

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/debug"
	"orchids-api/internal/logutil"
	"orchids-api/internal/opsagg"
	"orchids-api/internal/util"
)

// TraceIDHeader is the name of the HTTP header carrying the request trace ID.
const TraceIDHeader = "X-Trace-ID"

// RequestIDHeader is the name of the HTTP header carrying the request ID (an alias).
const RequestIDHeader = "X-Request-ID"

// traceIDKey is the context key the trace ID is stored under.
type traceIDKey struct{}
type requestIDKey struct{}

const DiagnosticRequestIDHeader = "X-Orchids-Request-ID"

// GenerateTraceID generates a new trace ID.
func GenerateTraceID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a timestamp.
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000")))
	}
	return hex.EncodeToString(b)
}

// TraceMiddleware adds request tracing.
// It reads the trace ID from the request headers and generates a new one when absent.
func TraceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := GenerateTraceID()
		// Client trace IDs may span retries; the server request ID never does.
		w.Header().Set(DiagnosticRequestIDHeader, requestID)
		// Try to read the trace ID from the request headers.
		traceID := r.Header.Get(TraceIDHeader)
		if traceID == "" {
			traceID = r.Header.Get(RequestIDHeader)
		}
		if traceID == "" {
			traceID = requestID
		}

		// Add the trace ID to the response headers. X-Request-ID is the header an
		// OpenAI-compatible SDK reads when it reports a failed request, so it is
		// echoed alongside the gateway's own trace headers.
		w.Header().Set(TraceIDHeader, traceID)
		w.Header().Set(RequestIDHeader, traceID)

		// Add the trace ID to the context.
		ctx := context.WithValue(r.Context(), traceIDKey{}, traceID)
		ctx = context.WithValue(ctx, requestIDKey{}, requestID)

		// Continue handling the request.
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// GetTraceID reads the trace ID from the context.
func GetTraceID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	traceID, _ := ctx.Value(traceIDKey{}).(string)
	return traceID
}

// GetRequestID is the per-HTTP-request journal and diagnostic identity.
func GetRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	if id == "" {
		return GetTraceID(ctx)
	}
	return id
}

// TracedResponseWriter wraps a ResponseWriter to record the response status.
type TracedResponseWriter struct {
	http.ResponseWriter
	StatusCode   int
	BytesWritten int64
	// firstWriteAt is when the handler produced its first byte.
	firstWriteAt time.Time
	// contentWriteAt ignores SSE control events, empty deltas and keepalives.
	// Non-streaming responses use the first body write as a TTFB approximation.
	contentWriteAt time.Time
	visibleWriteAt time.Time
	startedAt      time.Time
	tokenDetector  tokenSSEDetector
	// streamFailed records that the handler already committed a 2xx status and
	// then failed mid-stream, where the HTTP status can no longer say so.
	streamFailed bool
}

// NewTracedResponseWriter creates a new TracedResponseWriter.
func NewTracedResponseWriter(w http.ResponseWriter) *TracedResponseWriter {
	return &TracedResponseWriter{
		ResponseWriter: w,
		StatusCode:     http.StatusOK,
		startedAt:      time.Now(),
	}
}

// FirstWriteAt reports when the first response byte was produced, or the zero
// time when the handler never wrote anything.
func (w *TracedResponseWriter) FirstWriteAt() time.Time { return w.firstWriteAt }

// ContentWriteAt reports the first generated SSE content, or the first body
// write for non-streaming responses. A stream without generated output stays zero.
func (w *TracedResponseWriter) ContentWriteAt() time.Time { return w.contentWriteAt }

// MarkStreamFailure lets a streaming handler report a failure it found after the
// status line was already sent. The metric recorder reads it through
// StreamFailed(); the HTTP status stays whatever was committed, because it
// cannot be changed at that point.
func (w *TracedResponseWriter) MarkStreamFailure() {
	w.streamFailed = true
	MarkStreamFailure(w.ResponseWriter)
}

// StreamFailed reports whether the response failed after committing a 2xx status.
func (w *TracedResponseWriter) StreamFailed() bool { return w.streamFailed }

// streamFailureMarker is the capability a response writer exposes so a handler
// can flag a mid-stream failure without importing the concrete writer type.
type streamFailureMarker interface{ MarkStreamFailure() }

// MarkStreamFailure flags a mid-stream failure on whichever response writer the
// handler was given. It is a no-op for writers that do not support it (a test
// recorder, a direct call), so calling it is always safe.
func MarkStreamFailure(w http.ResponseWriter) {
	if marker, ok := w.(streamFailureMarker); ok && marker != nil {
		marker.MarkStreamFailure()
	}
}

// isPayloadWrite reports whether a write carries response payload rather than an
// SSE comment/keepalive ("": keepalive"). An empty or whitespace-only write is
// not payload either.
func isPayloadWrite(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case ':':
			// An SSE comment line (keepalive). Not payload.
			return false
		default:
			return true
		}
	}
	return false
}

// WriteHeader implements http.ResponseWriter.
func (w *TracedResponseWriter) WriteHeader(code int) {
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
	}
	w.StatusCode = code
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *TracedResponseWriter) Write(b []byte) (int, error) {
	now := time.Now()
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = now
	}
	n, err := w.ResponseWriter.Write(b)
	if n > 0 {
		payload := false
		if w.isSSE() {
			payload = w.tokenDetector.observe(b[:n])
			if w.visibleWriteAt.IsZero() && w.tokenDetector.visible {
				w.visibleWriteAt = now
			}
		} else {
			payload = isPayloadWrite(b[:n])
		}
		if payload && w.contentWriteAt.IsZero() {
			w.contentWriteAt = now
		}
	}
	w.BytesWritten += int64(n)
	return n, err
}

// Flush implements http.Flusher without losing the implicit HTTP 200 in the
// request metrics. A Flush commits the response even when no body was written.
func (w *TracedResponseWriter) Flush() {
	if w.firstWriteAt.IsZero() {
		w.firstWriteAt = time.Now()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController and capability checks reach the real
// server writer through every observability layer.
func (w *TracedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack implements http.Hijacker so WebSocket upgrades and similar cases keep working.
func (w *TracedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hj.Hijack()
}

// LoggingMiddleware logs requests, including the trace ID and the elapsed time.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		traceID := GetTraceID(r.Context())

		// Wrap the ResponseWriter.
		wrapped := NewTracedResponseWriter(w)
		wrapped.startedAt = start

		// The handler will publish the model it resolved; the hint must exist
		// before the handler runs because the request context is already cloned.
		requestCtx, readModel := RequestModelHint(r.Context())
		box := &requestObservation{}
		requestCtx = context.WithValue(requestCtx, requestObservationKey{}, box)
		requestCtx = util.WithHTTPPhaseObserver(requestCtx, func(phases map[string]int64) {
			box.mu.Lock()
			defer box.mu.Unlock()
			if box.httpPhases == nil {
				box.httpPhases = map[string]int64{}
			}
			box.httpPhaseSamples++
			for k, v := range phases {
				box.httpPhases[k] += v
			}
		})
		r = r.WithContext(requestCtx)

		logutil.DebugIf(logutil.VerboseDiagnosticsEnabled(), "Request started",
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
		)

		// Handle the request.
		next.ServeHTTP(wrapped, r)

		// Record the completed request.
		duration := time.Since(start)
		if capture := debug.FromContext(r.Context()); capture != nil {
			capture.Finish(duration)
		}
		finishRequestJournal(r, wrapped, duration, readModel())
		// The operations overview counts every finished request, so this record
		// must happen before the log level decides whether to print a line.
		recordRequestOutcome(r, wrapped, duration, readModel())

		level := slog.LevelDebug
		if wrapped.StatusCode >= 500 {
			level = slog.LevelError
		} else if wrapped.StatusCode >= 400 {
			level = slog.LevelWarn
		} else if !logutil.VerboseDiagnosticsEnabled() {
			return
		}
		userAgent := strings.TrimSpace(r.UserAgent())
		if len(userAgent) > 256 {
			userAgent = userAgent[:256]
		}

		slog.Log(r.Context(), level, "Request completed",
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.StatusCode,
			"bytes", wrapped.BytesWritten,
			"duration", duration,
			"remote_ip", ClientIP(r),
			"user_agent", userAgent,
		)
		// One request-finished event per request: a request retried upstream is
		// still one request, and no handler has to remember to record itself.
		// (The observation itself is taken above, before the log-level branch.)
	})
}

// requestModelContextKey carries the model a handler resolved for this request.
// The request path cannot know the model before the body is parsed (and the
// middleware must not re-read the body), so the handler publishes it on the
// context and the latency recorder picks it up when the request finishes.
type requestModelContextKey struct{}

// requestModelHintBox is a mutable slot: the handler runs after the middleware
// cloned the request context, so a plain context value would not be visible.
type requestModelHintBox struct {
	model string
}

// WithRequestModel publishes the resolved model for outcome recording. It is the
// only way the per-model figures in the operations overview get a name.
func WithRequestModel(ctx context.Context, model string) context.Context {
	if ctx == nil || strings.TrimSpace(model) == "" {
		return ctx
	}
	if box, ok := ctx.Value(requestModelContextKey{}).(*requestModelHintBox); ok && box != nil {
		box.model = strings.TrimSpace(model)
	}
	return ctx
}

// RequestModelHint returns a context that lets the wrapped handler publish its
// model, plus a reader for the value once the handler has run.
func RequestModelHint(ctx context.Context) (context.Context, func() string) {
	box := &requestModelHintBox{}
	return context.WithValue(ctx, requestModelContextKey{}, box), func() string { return box.model }
}

// RequestModelFromContext reads back the model published by an upstream
// dispatcher. A unified route has to know the model before the body reaches the
// real handler, so the dispatcher publishes it here and a downstream handler can
// reuse that single resolution instead of repeating a store lookup.
func RequestModelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	box, _ := ctx.Value(requestModelContextKey{}).(*requestModelHintBox)
	if box == nil {
		return ""
	}
	return box.model
}

func recordRequestOutcome(r *http.Request, wrapped *TracedResponseWriter, duration time.Duration, model string) {
	if detailedOutcomeRecorder == nil || r == nil || wrapped == nil {
		return
	}
	durationMS := duration.Milliseconds()
	// Only generated SSE output supplies TTFT. A failed or empty stream must
	// not fall back to headers and invent a first-token sample.
	firstWrite := wrapped.ContentWriteAt()
	if firstWrite.IsZero() && !wrapped.isSSE() {
		firstWrite = wrapped.FirstWriteAt()
	}
	firstTokenMS := int64(0)
	if !firstWrite.IsZero() && (wrapped.isSSE() || wrapped.StatusCode < 400) {
		firstTokenMS = firstWrite.Sub(wrapped.startedAt).Milliseconds()
		if firstTokenMS < 0 {
			firstTokenMS = 0
		}
	}
	statusClass := httpStatusClass(wrapped.StatusCode)
	if wrapped.StreamFailed() && statusClass == "2xx" {
		// The status line is already committed, so the only honest record of a
		// stream that died after it is a failure class of its own.
		statusClass = streamFailureClass
	}
	if detailedOutcomeRecorder != nil {
		outcome := opsagg.Outcome{Channel: inferenceRequestChannel(r), Model: model, Status: statusClass, HTTPStatus: wrapped.StatusCode, OK: statusClass == "2xx", DurationMS: durationMS, FirstTokenMS: firstTokenMS, At: time.Now(), Detailed: true}
		if wrapped.isSSE() {
			outcome.OutputEvidence = true
			outcome.VisibleOutput = wrapped.tokenDetector.visible
			outcome.ReasoningOnly = wrapped.tokenDetector.reasoning && !wrapped.tokenDetector.visible && !wrapped.tokenDetector.tool
			outcome.OutputTruncated = isTokenTruncation(wrapped.tokenDetector.finish)
		}
		if box, ok := r.Context().Value(requestObservationKey{}).(*requestObservation); ok {
			box.mu.Lock()
			if !wrapped.isSSE() && box.finalEvent != nil {
				meta := box.finalEvent.Metadata
				outcome.VisibleOutput, outcome.OutputEvidence = meta["visible_output"].(bool)
				outcome.ReasoningOnly, _ = meta["reasoning_only"].(bool)
				if reason, ok := meta["finish_reason"].(string); ok {
					outcome.OutputTruncated = isTokenTruncation(reason)
				}
			}
			outcome.InputTokens = box.input
			outcome.CachedTokens = box.cached
			outcome.OutputTokens = box.output
			outcome.ReasoningTokens = box.reasoning
			outcome.TotalTokens = box.total
			outcome.UsageReported = box.usage
			outcome.CostInUSDTicks = box.costTicks
			outcome.Priced = box.priced
			outcome.AttemptFailures = box.failures
			outcome.AccountSwitches = box.switches
			outcome.ProviderReached = box.providerReached
			box.mu.Unlock()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		detailedOutcomeRecorder(ctx, outcome)
	}
}

// Reserved synthetic-traffic labels. They are not routable models, so a client
// cannot use them to move its own traffic out of the real figures.
const HTTPChannel = "http"

// streamFailureClass is the status class recorded for a response that committed
// a 2xx status and then failed mid-stream. It counts as a failure and is kept
// distinct from a real 5xx, because the client saw an HTTP 200.
const streamFailureClass = "stream_error"

// requestChannel includes only routes that perform inference. Model discovery,
// token counting, administration, resource polling and downloads are HTTP traffic.
func requestChannel(path string) string {
	channelID, endpoint := channel.ID(""), ""
	if matched, ok := channel.FromPath(path); ok {
		channelID = matched
		definition, _ := channel.DefinitionFor(matched)
		endpoint = strings.TrimPrefix(path, definition.APIPrefix+"/")
	}
	channelName := string(channelID)
	if channelName == "" {
		if rest, ok := strings.CutPrefix(path, "/v1/"); ok {
			channelName, endpoint = string(channel.Grok), rest
		}
	}
	switch endpoint {
	case "messages", "chat/completions", "responses", "responses/compact":
		if channelName != "" {
			return channelName
		}
	}
	if channelName == "grok" {
		switch endpoint {
		case "responses", "responses/compact", "images/generations", "images/edits",
			"videos", "videos/generations", "videos/edits", "videos/extensions",
			"tts", "stt", "audio/speech", "audio/tasks", "audio/transcriptions", "realtime":
			return channelName
		}
	}
	return HTTPChannel
}

func inferenceRequestChannel(r *http.Request) string {
	channelName := requestChannel(r.URL.Path)
	if r.Method == http.MethodPost {
		return channelName
	}
	if r.Method == http.MethodGet && channelName == "grok" &&
		(strings.HasSuffix(r.URL.Path, "/stt") || strings.HasSuffix(r.URL.Path, "/realtime")) {
		return channelName
	}
	return HTTPChannel
}

// requestModelHint extracts a model from the request when it is cheap to do so.
// Request bodies are not re-read here: the log centre shows the model from the
// handler's own audit event, and the overview counts by channel.
func httpStatusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 300 && status < 400:
		return "3xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500 && status < 600:
		return "5xx"
	default:
		return "unknown"
	}
}

// Chain combines several middlewares into one chain.
func Chain(middlewares ...func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(final http.Handler) http.Handler {
		for i := len(middlewares) - 1; i >= 0; i-- {
			final = middlewares[i](final)
		}
		return final
	}
}
