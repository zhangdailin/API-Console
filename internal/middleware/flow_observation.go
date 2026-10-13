package middleware

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Reuse the outer trace writer so protocol bridges never parse SSE twice.
func flowTracedWriter(w http.ResponseWriter) *TracedResponseWriter {
	for w != nil {
		if traced, ok := w.(*TracedResponseWriter); ok {
			return traced
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = unwrapper.Unwrap()
	}
	return nil
}

func flowObservation(r *http.Request, w *TracedResponseWriter, now time.Time, panicked bool) (FlowCounts, flowLatencySample, [5]flowPhase) {
	counts := FlowCounts{Completed: 1}
	if w.StreamFailed() {
		counts.StreamErrors = 1
	}
	if w.StatusCode == 429 || w.StatusCode == 529 {
		counts.RateLimited = 1
	}
	if errors.Is(context.Cause(r.Context()), context.DeadlineExceeded) || w.StatusCode == http.StatusGatewayTimeout {
		counts.Timeouts = 1
	} else if r.Context().Err() != nil {
		counts.Canceled = 1
	}
	if panicked || w.StatusCode < 200 || w.StatusCode >= 300 || counts.StreamErrors > 0 || counts.Canceled > 0 || counts.Timeouts > 0 {
		counts.Failed = 1
	}
	sample := flowLatencySample{at: now, firstGeneration: -1, firstVisible: -1, bodyTTFB: -1, duration: now.Sub(w.startedAt).Milliseconds()}
	if w.StatusCode < 400 {
		if w.isSSE() {
			if !w.contentWriteAt.IsZero() {
				sample.firstGeneration = w.contentWriteAt.Sub(w.startedAt).Milliseconds()
			}
			if !w.visibleWriteAt.IsZero() {
				sample.firstVisible = w.visibleWriteAt.Sub(w.startedAt).Milliseconds()
			}
		} else if !w.contentWriteAt.IsZero() {
			sample.bodyTTFB = w.contentWriteAt.Sub(w.startedAt).Milliseconds()
		}
	}
	var phases [5]flowPhase
	if box, ok := r.Context().Value(requestObservationKey{}).(*requestObservation); ok {
		box.mu.Lock()
		defer box.mu.Unlock()
		counts.Observed = 1
		if box.reportedUsage {
			counts.UsageReported = 1
		}
		counts.Attempts, counts.AccountSwitches = box.attempts, box.switches
		if box.attempts > 1 {
			counts.Retried = 1
		}
		counts.RetryWaitMS, counts.QueueWaitMS = box.retryWaitMS, box.queueWaitMS
		for i, name := range flowPhaseNames {
			phases[i] = flowPhase{sum: box.httpPhases[name], samples: box.httpPhaseCounts[name]}
		}
	}
	return counts, sample, phases
}
