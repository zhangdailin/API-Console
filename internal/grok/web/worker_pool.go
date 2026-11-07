package web

import (
	"context"
	"sync"
	"sync/atomic"
)

// runWorkerPool runs process over items with at most workers goroutines. Items
// not started before ctx is done are passed to canceled instead (when non-nil).
func runWorkerPool[T any](ctx context.Context, items []T, workers int, process func(T), canceled func(T)) {
	workers = min(workers, len(items))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range max(0, workers) {
		wg.Go(func() {
			for {
				index := int(next.Add(1) - 1)
				if index >= len(items) {
					return
				}
				if ctx.Err() != nil {
					if canceled == nil {
						return
					}
					canceled(items[index])
				} else {
					process(items[index])
				}
			}
		})
	}
	wg.Wait()
}
