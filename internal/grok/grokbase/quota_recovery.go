package grokbase

import (
	"sync"
	"time"
)

// Recovery backoff bounds: the first re-read follows the exhaustion closely,
// later ones double up to the ceiling.
const (
	RecoveryBackoffBase = 30 * time.Second
	RecoveryBackoffMax  = 30 * time.Minute
	// recoveryClaimLease keeps a claimed account from being probed again while
	// its probe is still in flight (or its owner crashed mid-probe).
	recoveryClaimLease = 2 * time.Minute
)

type recoveryEntry struct {
	due      time.Time
	attempts int
}

// RecoveryQueue schedules control-plane re-reads of a spent allowance. An
// account whose quota is exhausted is otherwise released only when its passive
// cooldown expires, which can be hours after the upstream restored the
// allowance. The queue re-reads the quota surface instead: at the reset the
// upstream stated when it gave one, with bounded exponential backoff when it
// did not.
//
// The queue holds no account data, only due times, so it is rebuilt from the
// accounts themselves after a restart: the first tick that sees a spent
// account claims it immediately.
type RecoveryQueue struct {
	mu      sync.Mutex
	entries map[int64]recoveryEntry
}

// Claim reports whether the account's recovery probe is due now. A claimed
// account is leased so concurrent ticks do not probe it twice; the caller must
// follow with Reschedule or Done.
func (q *RecoveryQueue) Claim(accountID int64, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.entries == nil {
		q.entries = make(map[int64]recoveryEntry)
	}
	entry, ok := q.entries[accountID]
	if ok && now.Before(entry.due) {
		return false
	}
	entry.due = now.Add(recoveryClaimLease)
	q.entries[accountID] = entry
	return true
}

// Reschedule records a probe that found the allowance still spent (or could
// not read it) and returns when the next one is due. A reset time in the
// future wins over the backoff.
func (q *RecoveryQueue) Reschedule(accountID int64, now, resetAt time.Time) time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.entries == nil {
		q.entries = make(map[int64]recoveryEntry)
	}
	entry := q.entries[accountID]
	entry.attempts++
	if resetAt.After(now) {
		entry.due = resetAt
	} else {
		entry.due = now.Add(RecoveryBackoff(entry.attempts))
	}
	q.entries[accountID] = entry
	return entry.due
}

// Done forgets an account that recovered or left the exhausted state.
func (q *RecoveryQueue) Done(accountID int64) {
	q.mu.Lock()
	delete(q.entries, accountID)
	q.mu.Unlock()
}

// Pending reports whether the account is waiting for a recovery probe.
func (q *RecoveryQueue) Pending(accountID int64) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.entries[accountID]
	return ok
}

// RecoveryBackoff is the delay before the given (1-based) failed attempt's
// successor.
func RecoveryBackoff(attempt int) time.Duration {
	value := RecoveryBackoffBase
	for i := 1; i < attempt && value < RecoveryBackoffMax; i++ {
		value *= 2
	}
	return min(value, RecoveryBackoffMax)
}
