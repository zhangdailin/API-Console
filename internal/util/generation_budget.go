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
}

// WithFirstGenerationTimeout bounds pre-generation work across all attempts.
// MarkGenerationProgress stops only this timer; the parent's total deadline stays.
func WithFirstGenerationTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	b := &generationBudget{}
	ctx = context.WithValue(ctx, generationBudgetKey{}, b)
	if timeout > 0 {
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
