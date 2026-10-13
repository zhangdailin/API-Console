package api

import (
	"strings"
	"sync"
	"time"
)

// deviceLoginRegistry holds the in-flight device-authorization transactions for
// one channel.
//
// Every channel's login is the same machine: an id→transaction map guarded by a
// mutex, an admission limit, expiry cleanup, and get/cancel/finish accessors.
// Only the transaction type differs, so the storage and the bookkeeping live
// here once, and each channel supplies its element type plus the offset of the
// shared deviceLogin inside it. Before this existed each channel carried its own
// map, mutex and four accessors — the same forty lines four times.
type deviceLoginRegistry[T any] struct {
	mu     sync.Mutex
	logins map[string]*T
	// shared returns the embedded state the common bookkeeping operates on.
	shared func(*T) *deviceLogin
	// ready reports whether a transaction can still be polled. Most channels also
	// require a device code; Qoder does not, because its transaction exists before
	// the code is issued.
	ready func(*deviceLogin) bool
	// expiredMessage is the operator-facing reason a transaction timed out.
	expiredMessage string
}

func newDeviceLoginRegistry[T any](shared func(*T) *deviceLogin, ready func(*deviceLogin) bool, expiredMessage string) *deviceLoginRegistry[T] {
	if ready == nil {
		ready = deviceLoginReady
	}
	return &deviceLoginRegistry[T]{
		logins:         map[string]*T{},
		shared:         shared,
		ready:          ready,
		expiredMessage: expiredMessage,
	}
}

// deviceLoginReady is the default readiness test: the browser step is still
// outstanding and a device code has been issued.
func deviceLoginReady(login *deviceLogin) bool {
	return login != nil && login.status == "pending" && strings.TrimSpace(login.deviceCode) != ""
}

// deviceLoginReadyWithoutCode is Qoder's test. Its upstream issues the code during
// the first poll rather than at start, so requiring one here would refuse to poll
// the transaction that was just created.
func deviceLoginReadyWithoutCode(login *deviceLogin) bool {
	return login != nil && login.status == "pending"
}

// admit stores a new transaction unless the admission limit is reached. A refused
// transaction is not stored, so the caller owns cancelling it.
func (r *deviceLoginRegistry[T]) admit(id string, txn *T) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.logins) >= maxDeviceLogins {
		return false
	}
	r.logins[id] = txn
	return true
}

// response renders the wire response for one transaction, and reports whether it
// exists. It reads the state under the lock so a concurrent finish cannot be
// observed half-applied.
func (r *deviceLoginRegistry[T]) response(id string) (deviceLoginResponse, bool) {
	if r == nil {
		return deviceLoginResponse{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	txn := r.logins[id]
	if txn == nil {
		return deviceLoginResponse{}, false
	}
	return newDeviceLoginResponse(id, r.shared(txn)), true
}

// cancel removes a transaction and marks it cancelled. It returns the channel the
// transaction's poll loop closes when it stops, so the caller can wait for that
// loop to finish before answering the request.
func (r *deviceLoginRegistry[T]) cancel(id, message string) (<-chan struct{}, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	txn := r.logins[id]
	if txn == nil {
		return nil, false
	}
	login := r.shared(txn)
	delete(r.logins, id)
	finishDeviceLogin(login, "cancelled", message, 0)
	if login == nil {
		return nil, true
	}
	return login.done, true
}

// pollable returns a copy of the transaction while it is still pollable. The copy
// is deliberate: the poll loop reads several fields after the lock is released,
// and a shared pointer would let a concurrent cancel mutate them underneath it.
func (r *deviceLoginRegistry[T]) pollable(id string) (*T, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	txn := r.logins[id]
	if txn == nil || !r.ready(r.shared(txn)) {
		return nil, false
	}
	copied := *txn
	return &copied, true
}

// pending reports whether the transaction is still awaiting the browser step.
func (r *deviceLoginRegistry[T]) pending(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	txn := r.logins[id]
	if txn == nil {
		return false
	}
	login := r.shared(txn)
	return login != nil && login.status == "pending"
}

// update runs fn on the stored transaction under the lock, for channel state that
// is not part of the shared bookkeeping. Grok widens its poll interval here when
// the upstream asks it to slow down.
func (r *deviceLoginRegistry[T]) update(id string, fn func(*T)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if txn := r.logins[id]; txn != nil {
		fn(txn)
	}
}

// finish records a transaction's outcome without removing it, so the console can
// still read the result.
func (r *deviceLoginRegistry[T]) finish(id, status, message string, accountID int64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	txn := r.logins[id]
	if txn == nil {
		return
	}
	finishDeviceLogin(r.shared(txn), status, message, accountID)
}

// cleanup expires timed-out transactions and drops entries that are long past
// their expiry, so a stalled flow cannot hold a slot of the admission limit.
func (r *deviceLoginRegistry[T]) cleanup(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, txn := range r.logins {
		login := r.shared(txn)
		if login == nil {
			delete(r.logins, id)
			continue
		}
		if login.status == "pending" && now.After(login.expiresAt) {
			finishDeviceLogin(login, "expired", r.expiredMessage, 0)
		}
		if now.After(login.expiresAt.Add(15 * time.Minute)) {
			delete(r.logins, id)
		}
	}
}

// identityDeviceLogin is the shared accessor for a registry whose element *is* a
// deviceLogin (the Grok alias).
func identityDeviceLogin(login *deviceLogin) *deviceLogin { return login }
