// Package util provides general-purpose helpers.
package util

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrTaskPanic never exposes recovered values, which may contain secrets.
var ErrTaskPanic = errors.New("task panicked")

// RunIndexed executes each index once and returns errors in input order.
// Serial and parallel execution both isolate panics and finish other tasks.
func RunIndexed(total, workers int, work func(int) error) []error {
	if total <= 0 || work == nil {
		return nil
	}
	workers = max(1, min(workers, total))
	errs := make([]error, total)
	run := func(index int) {
		defer func() {
			if recover() != nil {
				errs[index] = ErrTaskPanic
			}
		}()
		errs[index] = work(index)
	}
	if workers == 1 {
		for index := range total {
			run(index)
		}
		return errs
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for index := range jobs {
				run(index)
			}
		}()
	}
	for index := range total {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return errs
}

// SleepWithContext is a cancellable sleep; false means the context was cancelled.
func SleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
