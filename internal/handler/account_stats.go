package handler

import (
	"context"
	"fmt"
	"log/slog"
	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
	"time"
)

type accountStatsDelta struct {
	accountID   int64
	usage       float64
	count       int64
	operationID string
	completedAt time.Time
}

func (h *Handler) initAccountStatsWriter() {
	h.statsPending = make(map[string]accountStatsDelta)
	h.statsWake = make(chan struct{}, 1)
	h.statsStop = make(chan struct{})
	h.statsDone = make(chan struct{})
	go h.runAccountStatsWriter()
}

func (h *Handler) updateAccountStats(ctx context.Context, account *store.Account, inputTokens, outputTokens int) {
	if account == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	h.statsOnce.Do(h.initAccountStatsWriter)
	completedAt := time.Now().UTC()
	requestID := middleware.GetRequestID(ctx)
	operationID := fmt.Sprintf("%d:%s", account.ID, requestID)
	if requestID == "" {
		operationID = fmt.Sprintf("stats-%d-%d", account.ID, completedAt.UnixNano())
	}
	delta := accountStatsDelta{accountID: account.ID, usage: float64(inputTokens + outputTokens), count: 1, operationID: operationID, completedAt: completedAt}
	h.statsMu.Lock()
	if h.statsClosed {
		h.statsMu.Unlock()
		return
	}
	// Keep each completion as its own durable operation. Coalescing by account
	// loses the request identity needed to make an ambiguous Redis retry safe.
	h.statsPending[operationID] = delta
	h.statsMu.Unlock()
	select {
	case h.statsWake <- struct{}{}:
	default:
	}
}

// popPendingStats takes one queued completion out of the pending map. It reports
// false when the queue is empty.
func (h *Handler) popPendingStats() (string, accountStatsDelta, bool) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	for key, pending := range h.statsPending {
		delete(h.statsPending, key)
		return key, pending, true
	}
	return "", accountStatsDelta{}, false
}

func (h *Handler) runAccountStatsWriter() {
	defer close(h.statsDone)
	const (
		initialBackoff = 100 * time.Millisecond
		maxBackoff     = 5 * time.Second
	)
	backoff := initialBackoff
	for {
		select {
		case <-h.statsStop:
			return
		case <-h.statsWake:
		}
		for {
			pendingKey, delta, found := h.popPendingStats()
			if !found {
				backoff = initialBackoff
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.loadBalancer.Store.IncrementAccountStatsOperation(ctx, delta.accountID, delta.usage, delta.count, delta.operationID, delta.completedAt)
			cancel()
			if err == nil {
				backoff = initialBackoff
				continue
			}

			// Requeue the identical operation. If Redis committed before the caller
			// observed an error, the durable operation id makes this retry a no-op.
			h.statsMu.Lock()
			h.statsPending[pendingKey] = delta
			h.statsMu.Unlock()
			slog.Error("Failed to update account stats; retrying", "account_id", delta.accountID, "operation_id", delta.operationID, "retry_in", backoff, "error", err)
			timer := time.NewTimer(backoff)
			select {
			case <-h.statsStop:
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}
}

func (h *Handler) flushPendingAccountStats(timeout time.Duration) {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pendingKey, delta, found := h.popPendingStats()
		if !found {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		err := h.loadBalancer.Store.IncrementAccountStatsOperation(ctx, delta.accountID, delta.usage, delta.count, delta.operationID, delta.completedAt)
		cancel()
		if err != nil {
			h.statsMu.Lock()
			h.statsPending[pendingKey] = delta
			h.statsMu.Unlock()
			slog.Warn("account stats remain unflushed during shutdown", "account_id", delta.accountID, "operation_id", delta.operationID, "error", err)
			return
		}
	}
}
