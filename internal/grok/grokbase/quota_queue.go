package grokbase

import (
	"net/http"
	"sync"

	"orchids-api/internal/store"
)

// QuotaJob is the coalesced quota observation of one account: the latest
// response headers and how many requests were served since the last write.
type QuotaJob struct {
	Account  store.Account
	Headers  http.Header
	Requests int64
}

// QuotaQueue moves quota bookkeeping off the request path. Observations for
// the same account coalesce into one job, and a single worker persists them,
// so a burst of responses costs one Redis round trip per account.
type QuotaQueue struct {
	once    sync.Once
	mu      sync.Mutex
	pending map[int64]QuotaJob
	wake    chan struct{}
}

// Enqueue records one served response for acc. persist runs on the queue's
// worker; every call must pass the same function.
func (q *QuotaQueue) Enqueue(acc *store.Account, headers http.Header, persist func(QuotaJob)) {
	if acc == nil {
		return
	}
	q.once.Do(func() {
		q.pending = make(map[int64]QuotaJob)
		q.wake = make(chan struct{}, 1)
		go q.run(persist)
	})
	q.mu.Lock()
	job := q.pending[acc.ID]
	job.Account = *acc
	job.Headers = headers.Clone()
	job.Requests++
	q.pending[acc.ID] = job
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *QuotaQueue) run(persist func(QuotaJob)) {
	for range q.wake {
		for {
			q.mu.Lock()
			var (
				accountID int64
				job       QuotaJob
			)
			for id, pending := range q.pending {
				accountID, job = id, pending
				delete(q.pending, id)
				break
			}
			q.mu.Unlock()
			if accountID == 0 {
				break
			}
			persist(job)
		}
	}
}
