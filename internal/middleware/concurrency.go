package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"
)

// ConcurrencyLimiter limits concurrent request processing using a weighted semaphore.
// This is more efficient than channel-based semaphore for high-throughput scenarios.
type ConcurrencyLimiter struct {
	activeCount  atomic.Int64
	rejectedReqs atomic.Int64

	sem     *semaphore.Weighted
	timeout time.Duration
}

type concurrencyAdmissionKey struct{}

// NewConcurrencyLimiter creates a limiter with fixed capacity and timeout.
func NewConcurrencyLimiter(maxConcurrent int, timeout time.Duration) *ConcurrencyLimiter {
	if maxConcurrent <= 0 {
		maxConcurrent = 100
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &ConcurrencyLimiter{
		sem:     semaphore.NewWeighted(int64(maxConcurrent)),
		timeout: timeout,
	}
}

// Limit admits a request only when a concurrency slot is free, and bounds the
// handler's execution with the limiter timeout.
func (cl *ConcurrencyLimiter) Limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if admitted, _ := r.Context().Value(concurrencyAdmissionKey{}).(*ConcurrencyLimiter); admitted == cl {
			next.ServeHTTP(w, r)
			return
		}
		// Admission is deliberately non-blocking. Queueing requests behind the
		// semaphore consumes connections and goroutines precisely when the server
		// is overloaded, which can amplify an overload into a broader outage.
		if !cl.sem.TryAcquire(1) {
			cl.rejectedReqs.Add(1)
			// Log bounded samples; logging every rejected request worsens overload.
			if count := cl.rejectedReqs.Load(); count == 1 || count%1000 == 0 {
				slog.Warn("Concurrency limit: Request rejected", "total_rejected", count)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"server is overloaded; retry later","type":"server_error","code":"server_overloaded","param":null}}`))
			return
		}

		slog.Debug("Concurrency limit: Slot acquired", "active", cl.activeCount.Load()+1)

		cl.activeCount.Add(1)
		reqStart := time.Now()

		defer func() {
			cl.sem.Release(1)
			cl.activeCount.Add(-1)

			duration := time.Since(reqStart)
			slog.Debug("Concurrency limit: Slot released", "active", cl.activeCount.Load(), "duration", duration)
		}()

		// Use the full concurrency timeout for ordinary request execution.
		execCtx, cancelExec := context.WithTimeout(r.Context(), cl.timeout)
		execCtx = context.WithValue(execCtx, concurrencyAdmissionKey{}, cl)
		defer cancelExec()
		slog.Debug("Concurrency limit: Serving request", "path", r.URL.Path, "timeout", cl.timeout)
		next.ServeHTTP(w, r.WithContext(execCtx))
	}
}
