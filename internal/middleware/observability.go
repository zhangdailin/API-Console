package middleware

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"orchids-api/internal/audit"
	"orchids-api/internal/debug"
	"orchids-api/internal/opsagg"
)

type requestObservationKey struct{}
type requestObservation struct {
	mu                                      sync.Mutex
	input, cached, output, reasoning, total int64
	usage                                   bool
	// costTicks sums every priced row the request produced, and priced marks that
	// at least one of them carried a price.
	costTicks                    int64
	priced                       bool
	attempts, failures, switches int64
	account                      int64
	providerReached              bool
	retryWaitMS, queueWaitMS     int64
	httpPhaseSamples             int64
	httpPhases                   map[string]int64
	finalEvent                   *audit.Event
	journal                      audit.Logger
}

var requestJournal audit.Logger

func SetRequestAuditLogger(logger audit.Logger) { requestJournal = logger }

var detailedOutcomeRecorder func(context.Context, opsagg.Outcome)

func SetDetailedOutcomeRecorder(recorder func(context.Context, opsagg.Outcome)) {
	detailedOutcomeRecorder = recorder
}

type observedAuditLogger struct{ next audit.Logger }

func ObserveAuditLogger(next audit.Logger) audit.Logger { return observedAuditLogger{next} }
func (l observedAuditLogger) Log(ctx context.Context, e audit.Event) {
	deferJournal := false
	if box, ok := ctx.Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		// A request can produce more than one priced row (a retry, or a media
		// call after a text attempt), so the cost is summed rather than replaced.
		if e.CostInUSDTicks > 0 {
			box.costTicks += e.CostInUSDTicks
			box.priced = true
		}
		if strings.HasSuffix(e.Action, "upstream_attempt") {
			box.providerReached = true
			box.attempts++
			if e.Status != "ok" && e.Status != "success" {
				box.failures++
			}
			if box.account != 0 && e.AccountID != 0 && box.account != e.AccountID {
				box.switches++
			}
			if e.AccountID != 0 {
				box.account = e.AccountID
			}
			// Native Responses reports its usage on the completed upstream attempt.
			if e.UsageSource == audit.UsageSourceUpstream || e.InputTokens > 0 || e.OutputTokens > 0 {
				box.input += int64(e.InputTokens)
				box.cached += int64(e.CachedInputTokens)
				box.output += int64(e.OutputTokens)
				box.reasoning += int64(e.ReasoningTokens)
				box.total += int64(e.TotalTokens)
				box.usage = true
			}
		} else if e.Action == "chat_request" || e.Action == "grok_request" {
			copy := e
			box.finalEvent, box.journal = &copy, l.next
			deferJournal = true
			box.providerReached = true
			if e.UsageSource == audit.UsageSourceUpstream || e.InputTokens > 0 || e.OutputTokens > 0 {
				box.input = int64(e.InputTokens)
				box.cached = int64(e.CachedInputTokens)
				box.output = int64(e.OutputTokens)
				box.reasoning = int64(e.ReasoningTokens)
				box.total = int64(e.TotalTokens)
				box.usage = true
			}
		}
		box.mu.Unlock()
	}
	if deferJournal {
		return
	}
	if id := GetRequestID(ctx); id != "" {
		e.RequestID = id
	}
	if capture := debug.FromContext(ctx); capture != nil {
		raw, _ := json.Marshal(e)
		capture.Append("6_request_events.jsonl", string(raw)+"\n")
	}
	if l.next != nil {
		l.next.Log(ctx, e)
	}
}

// Diagnostics captures only inference traffic. Tee readers preserve streaming
// and body limits; storage failures never replace a successful client response.
type DiagnosticBudget struct{ SampleEvery, MaxConcurrent int }

func Diagnostics(store *debug.DiagnosticStore, enabled func() bool, budgets ...func() DiagnosticBudget) func(http.Handler) http.Handler {
	var counter, active atomic.Int64
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if store == nil || enabled == nil || !enabled() || r.Method != http.MethodPost || requestChannel(r.URL.Path) == HTTPChannel {
				next.ServeHTTP(w, r)
				return
			}
			contentType := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
			if contentType != "" && !strings.Contains(contentType, "application/json") {
				next.ServeHTTP(w, r)
				return
			}
			budget := DiagnosticBudget{SampleEvery: 1, MaxConcurrent: 8}
			if len(budgets) > 0 && budgets[0] != nil {
				budget = budgets[0]()
			}
			if budget.SampleEvery <= 0 {
				budget.SampleEvery = 1
			}
			if budget.MaxConcurrent <= 0 {
				budget.MaxConcurrent = 8
			}
			if (counter.Add(1)-1)%int64(budget.SampleEvery) != 0 {
				w.Header().Set("X-Diagnostic-Capture", "sampled-out")
				next.ServeHTTP(w, r)
				return
			}
			for {
				n := active.Load()
				if n >= int64(budget.MaxConcurrent) {
					w.Header().Set("X-Diagnostic-Capture", "budget-exhausted")
					next.ServeHTTP(w, r)
					return
				}
				if active.CompareAndSwap(n, n+1) {
					break
				}
			}
			defer active.Add(-1)
			w.Header().Set("X-Diagnostic-Capture", "enabled")
			ctx, capture := debug.WithCapture(r.Context(), GetRequestID(r.Context()))
			r = r.WithContext(ctx)
			if r.Body != nil {
				r.Body = &diagnosticReader{ReadCloser: r.Body, capture: capture, name: "1_http_request.json"}
			}
			defer capture.Close()
			writer := &diagnosticWriter{TracedResponseWriter: NewTracedResponseWriter(w), capture: capture}
			defer func() {
				capture.Set("6_http_summary.json", fmtJSON(map[string]interface{}{"status": writer.StatusCode, "bytes": writer.BytesWritten, "stream_failed": writer.StreamFailed()}))
				writer.saveResponseDiagnostic()
				saveCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := store.Save(saveCtx, capture.Bundle()); err != nil {
					slog.Warn("Could not save request diagnostics", "error", err)
				}
			}()
			next.ServeHTTP(writer, r)
		})
	}
}
func fmtJSON(value interface{}) string { raw, _ := json.Marshal(value); return string(raw) }

type diagnosticReader struct {
	io.ReadCloser
	capture *debug.Capture
	name    string
}

func (r *diagnosticReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.capture.Append(r.name, string(p[:n]))
	return n, err
}

type diagnosticWriter struct {
	*TracedResponseWriter
	capture *debug.Capture
}

func (w *diagnosticWriter) Write(p []byte) (int, error) {
	n, err := w.TracedResponseWriter.Write(p)
	if n > 0 {
		w.capture.Append("5_http_response.txt", string(p[:n]))
	}
	return n, err
}
func (w *diagnosticWriter) saveResponseDiagnostic() {
	raw, _ := json.Marshal(map[string]interface{}{"status": w.StatusCode, "content_type": w.Header().Get("Content-Type"), "bytes": w.BytesWritten, "stream_failed": w.StreamFailed()})
	w.capture.Set("5_http_response_metadata.json", string(raw))
}

func RecordUpstreamAttempt(ctx context.Context, accountID int64, failed bool) {
	if box, ok := ctx.Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		box.providerReached = true
		box.attempts++
		if failed {
			box.failures++
		}
		if box.account != 0 && accountID != 0 && box.account != accountID {
			box.switches++
		}
		if accountID != 0 {
			box.account = accountID
		}
	}
}

// Complete exactly one request journal row, including failures before a handler
// reaches its normal audit call. Use the same duration as the metrics/capture.
func finishRequestJournal(r *http.Request, w *TracedResponseWriter, duration time.Duration, model string) {
	channel := inferenceRequestChannel(r)
	if channel == HTTPChannel {
		return
	}
	logger := requestJournal
	e := audit.Event{Kind: audit.KindRequest, Action: "http_request", Channel: channel, Model: model}
	if box, ok := r.Context().Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		if box.finalEvent != nil {
			e = *box.finalEvent
		}
		if box.journal != nil {
			logger = box.journal
		}
		box.mu.Unlock()
	}
	if logger == nil {
		return
	}
	e.RequestID, e.Duration = GetRequestID(r.Context()), duration.Milliseconds()
	e.Timestamp = time.Now()
	e.ClientIP, e.UserAgent = ClientIP(r), r.UserAgent()
	metadata := map[string]interface{}{}
	for k, v := range e.Metadata {
		metadata[k] = v
	}
	metadata["http_status"], metadata["path"], metadata["method"] = w.StatusCode, r.URL.Path, r.Method
	metadata["trace_id"] = GetTraceID(r.Context())
	if !w.FirstWriteAt().IsZero() {
		metadata["ttfb_ms"] = w.FirstWriteAt().Sub(w.startedAt).Milliseconds()
	}
	if !w.ContentWriteAt().IsZero() && (w.isSSE() || w.StatusCode < 400) {
		metadata["first_token_ms"] = w.ContentWriteAt().Sub(w.startedAt).Milliseconds()
		if !w.isSSE() {
			metadata["first_token_kind"] = "body_ttfb"
		}
	}
	if w.isSSE() {
		metadata["visible_output"] = w.tokenDetector.visible
		metadata["reasoning_only"] = w.tokenDetector.reasoning && !w.tokenDetector.visible && !w.tokenDetector.tool
		if w.tokenDetector.finish != "" {
			metadata["finish_reason"] = w.tokenDetector.finish
		}
		if !w.visibleWriteAt.IsZero() {
			metadata["first_visible_token_ms"] = w.visibleWriteAt.Sub(w.startedAt).Milliseconds()
		}
	}
	if reason, ok := metadata["finish_reason"].(string); ok {
		metadata["output_truncated"] = isTokenTruncation(reason)
	}
	if box, ok := r.Context().Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		metadata["upstream_http_phase_samples"] = box.httpPhaseSamples
		phaseSnapshot := make(map[string]int64, len(box.httpPhases))
		for k, v := range box.httpPhases {
			phaseSnapshot[k] = v
		}
		metadata["upstream_http_phases_sum"] = phaseSnapshot
		metadata["retry_wait_ms"] = box.retryWaitMS
		metadata["queue_wait_ms"] = box.queueWaitMS
		metadata["upstream_attempts"] = box.attempts
		metadata["account_switches"] = box.switches
		box.mu.Unlock()
	}
	e.Metadata = metadata
	if w.StreamFailed() {
		e.Status = "stream_error"
	} else if w.StatusCode >= 400 {
		e.Status = "error"
	} else if e.Status == "" {
		e.Status = "success"
	}
	if capture := debug.FromContext(r.Context()); capture != nil {
		capture.Append("6_request_events.jsonl", fmtJSON(e)+"\n")
	}
	logger.Log(r.Context(), e)
}

// RecordRetryWait records lightweight wait evidence even with diagnostic capture off.
func RecordRetryWait(ctx context.Context, category string, elapsed time.Duration) {
	if box, ok := ctx.Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		box.retryWaitMS += elapsed.Milliseconds()
		if category == "upstream_queue" {
			box.queueWaitMS += elapsed.Milliseconds()
		}
	}
}
