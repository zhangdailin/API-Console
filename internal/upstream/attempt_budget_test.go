package upstream

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestAttemptBudgetIsAtomicAndRecordsEachActualCallOnce(t *testing.T) {
	ctx, b := WithAttemptBudget(context.Background(), 4)
	var recorded, accepted atomic.Int32
	ctx = WithAttemptObserver(ctx, func(bool) { recorded.Add(1) })
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finish, err := BeginAttempt(ctx)
			if err != nil {
				if !errors.Is(err, ErrAttemptBudget) {
					t.Error(err)
				}
				return
			}
			accepted.Add(1)
			finish(nil)
			finish(nil)
		}()
	}
	wg.Wait()
	if accepted.Load() != 4 || recorded.Load() != 4 || b.Remaining() != 0 {
		t.Fatalf("accepted=%d recorded=%d remaining=%d", accepted.Load(), recorded.Load(), b.Remaining())
	}
}
