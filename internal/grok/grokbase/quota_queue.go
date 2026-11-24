package grokbase

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

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

// Persist records usage, then applies the latest observed quota to a fresh
// account row. With no upstream observation, consume the local allowance.
func (j QuotaJob) Persist(s *store.Store, provider string, observed bool, apply func(*store.Account) bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := j.Account.ID
	if err := s.IncrementAccountStats(ctx, id, 0, j.Requests); err != nil {
		slog.Warn("grok usage touch failed", "account_id", id, "error", err)
	}
	if !observed {
		if _, err := s.ConsumeGrokQuota(ctx, id, provider, float64(j.Requests)); err != nil {
			slog.Warn("grok local quota decrement failed", "account_id", id, "error", err)
		}
		return
	}
	account, err := s.GetAccount(ctx, id)
	if err != nil || account == nil {
		slog.Warn("grok quota account reload failed", "account_id", id, "error", err)
		return
	}
	if apply(account) {
		if err := s.UpdateAccount(ctx, account); err != nil {
			slog.Warn("grok quota update failed", "account_id", id, "error", err)
		}
	}
}
