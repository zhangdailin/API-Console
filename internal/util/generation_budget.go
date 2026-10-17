package util

import (
	"context"
	"sync"
	"time"
)

type generationBudgetKey struct{}
type generationBudget struct {
	mu       sync.Mutex
	timer    *time.Timer
	finished bool
	deadline time.Time
}

// WithFirstGenerationTimeout bounds pre-generation work across all attempts.
// MarkGenerationProgress stops only this timer; the parent's total deadline stays.
func WithFirstGenerationTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	b := &generationBudget{}
	ctx = context.WithValue(ctx, generationBudgetKey{}, b)
	if timeout > 0 {
		b.deadline = time.Now().Add(timeout)
		b.timer = time.AfterFunc(timeout, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if !b.finished {
				b.finished = true
				cancel(context.DeadlineExceeded)
			}
		})
	}
	return ctx, func() { MarkGenerationProgress(ctx); cancel(context.Canceled) }
}

// RetryWaitFits rejects waits that cannot finish before an active request or
// first-generation deadline. It never shortens an upstream Retry-After hint.
func RetryWaitFits(ctx context.Context, wait time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	end := time.Now().Add(max(wait, 0))
	if deadline, ok := ctx.Deadline(); ok && !end.Before(deadline) {
		return false
	}
	if b, ok := ctx.Value(generationBudgetKey{}).(*generationBudget); ok {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.finished && !b.deadline.IsZero() && !end.Before(b.deadline) {
			return false
		}
	}
	return true
}

func MarkGenerationProgress(ctx context.Context) {
	b, _ := ctx.Value(generationBudgetKey{}).(*generationBudget)
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finished = true
	if b.timer != nil {
		b.timer.Stop()
	}
}
