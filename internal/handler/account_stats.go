package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	accountStatsQueueLimit = 4096
	// This best-effort queue must stop retrying well before Redis forgets the
	// operation ID, otherwise a delayed retry could double-count a completion.
	accountStatsRetryWindow = 10 * time.Minute
)

var accountStatsDropped = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "orchids_account_stats_dropped_total",
	Help: "Account usage observations discarded by the background statistics writer.",
}, []string{"reason"})

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
	if !h.enqueueAccountStats(delta) {
		return
	}
	select {
	case h.statsWake <- struct{}{}:
	default:
	}
}

func (h *Handler) enqueueAccountStats(delta accountStatsDelta) bool {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	if h.statsClosed {
		return false
	}
	if _, pending := h.statsPending[delta.operationID]; pending {
		return false
	}
	if len(h.statsPending) >= accountStatsQueueLimit {
		accountStatsDropped.WithLabelValues("queue_full").Inc()
		return false
	}
	// Keep each completion as its own durable operation. Coalescing by account
	// loses the request identity needed to make an ambiguous Redis retry safe.
	h.statsPending[delta.operationID] = delta
	return true
}

// nextAccountStats keeps the selected operation in the map until it settles,
// so the in-flight writer shares the same limit and cannot overwrite a retry.
func (h *Handler) nextAccountStats() (accountStatsDelta, bool) {
	h.statsMu.Lock()
	defer h.statsMu.Unlock()
	for _, pending := range h.statsPending {
		return pending, true
	}
	return accountStatsDelta{}, false
}

func (h *Handler) applyPendingAccountStats(ctx context.Context, delta accountStatsDelta) error {
	dropReason := ""
	if time.Since(delta.completedAt) >= accountStatsRetryWindow {
		dropReason = "expired"
	} else {
		err := h.loadBalancer.Store.IncrementAccountStatsOperation(ctx, delta.accountID, delta.usage, delta.count, delta.operationID, delta.completedAt)
		if errors.Is(err, store.ErrNoRows) {
			dropReason = "account_missing"
		} else if err != nil {
			// Redis might have committed before the response was lost. Keep the
			// exact same operation for retry rather than assigning a fresh ID.
			return err
		}
	}
	h.statsMu.Lock()
	delete(h.statsPending, delta.operationID)
	h.statsMu.Unlock()
	if dropReason != "" {
		accountStatsDropped.WithLabelValues(dropReason).Inc()
	}
	return nil
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
			select {
			case <-h.statsStop:
				return
			default:
			}
			delta, found := h.nextAccountStats()
			if !found {
				backoff = initialBackoff
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.applyPendingAccountStats(ctx, delta)
			cancel()
			if err == nil {
				backoff = initialBackoff
				continue
			}
			slog.Error("Failed to update account stats; retrying", "account_id", delta.accountID, "operation_id", delta.operationID, "retry_in", backoff, "error", err)
			timer := time.NewTimer(backoff)
			select {
			case <-h.statsStop:
				timer.Stop()
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
	defer func() {
		h.statsMu.Lock()
		remaining := len(h.statsPending)
		clear(h.statsPending)
		h.statsMu.Unlock()
		if remaining > 0 {
			accountStatsDropped.WithLabelValues("shutdown").Add(float64(remaining))
		}
	}()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		delta, found := h.nextAccountStats()
		if !found {
			return
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		ctx, cancel := context.WithTimeout(context.Background(), remaining)
		err := h.applyPendingAccountStats(ctx, delta)
		cancel()
		if err != nil {
			slog.Warn("Account statistics remain unflushed during shutdown", "account_id", delta.accountID, "operation_id", delta.operationID, "error", err)
			return
		}
	}
}
