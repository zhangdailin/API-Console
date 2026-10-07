package upstream

import (
	"context"
	"errors"
	"sync/atomic"
)

type attemptBudgetKey struct{}
type attemptObserverKey struct{}

// AttemptBudget counts actual inference HTTP calls, including authentication replays.
// Control-plane refresh grants do not consume the generation budget.
type AttemptBudget struct {
	limit int64
	used  atomic.Int64
}

var ErrAttemptBudget = errors.New("upstream attempt budget exhausted")

func WithAttemptBudget(ctx context.Context, limit int) (context.Context, *AttemptBudget) {
	if limit < 1 {
		limit = 1
	}
	b := &AttemptBudget{limit: int64(limit)}
	return context.WithValue(ctx, attemptBudgetKey{}, b), b
}

func (b *AttemptBudget) Used() int64      { return b.used.Load() }
func (b *AttemptBudget) Remaining() int64 { return b.limit - b.Used() }

// RemainingAttempts returns -1 for callers without a shared request budget.
func RemainingAttempts(ctx context.Context) int64 {
	if b, ok := ctx.Value(attemptBudgetKey{}).(*AttemptBudget); ok {
		return b.Remaining()
	}
	return -1
}

func WithAttemptObserver(ctx context.Context, observer func(bool)) context.Context {
	return context.WithValue(ctx, attemptObserverKey{}, observer)
}

// BeginAttempt must be called immediately before sending an inference request.
// Its finish callback records the outcome once, including repaired 401 attempts.
func BeginAttempt(ctx context.Context) (func(error), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b, ok := ctx.Value(attemptBudgetKey{}).(*AttemptBudget); ok {
		for {
			n := b.used.Load()
			if n >= b.limit {
				return nil, ErrAttemptBudget
			}
			if b.used.CompareAndSwap(n, n+1) {
				break
			}
		}
	}
	var done atomic.Bool
	return func(err error) {
		if done.CompareAndSwap(false, true) {
			if observer, ok := ctx.Value(attemptObserverKey{}).(func(bool)); ok {
				observer(err != nil)
			}
		}
	}, nil
}
