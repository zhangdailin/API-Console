package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/channel"
)

// deviceLoginRegistry owns the in-flight transactions for one channel.
type deviceLoginRegistry struct {
	mu             sync.Mutex
	logins         map[string]*deviceLogin
	expiredMessage string
}

func newDeviceLoginRegistry(id channel.ID) *deviceLoginRegistry {
	return &deviceLoginRegistry{
		logins:         make(map[string]*deviceLogin),
		expiredMessage: channelLabel(id) + " authorization expired",
	}
}

var errDeviceLoginLimit = errors.New("too many pending device logins")

// start snapshots the public response before polling can mutate the transaction.
func (r *deviceLoginRegistry) start(login *deviceLogin, poll func(context.Context, string)) (deviceLoginResponse, error) {
	id, err := newDeviceLoginID()
	if err != nil {
		return deviceLoginResponse{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	login.cancel, login.done = cancel, make(chan struct{})
	response := newDeviceLoginResponse(id, login)
	if !r.admit(id, login) {
		cancel()
		return deviceLoginResponse{}, errDeviceLoginLimit
	}
	go func() {
		defer close(login.done)
		poll(ctx, id)
	}()
	return response, nil
}

// admit stores a new transaction unless the admission limit is reached. A refused
// transaction is not stored, so the caller owns cancelling it.
func (r *deviceLoginRegistry) admit(id string, login *deviceLogin) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.logins) >= maxDeviceLogins {
		return false
	}
	r.logins[id] = login
	return true
}

// response renders the wire response for one transaction, and reports whether it
// exists. It reads the state under the lock so a concurrent finish cannot be
// observed half-applied.
func (r *deviceLoginRegistry) response(id string) (deviceLoginResponse, bool) {
	if r == nil {
		return deviceLoginResponse{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	login := r.logins[id]
	if login == nil {
		return deviceLoginResponse{}, false
	}
	return newDeviceLoginResponse(id, login), true
}

// cancel removes a transaction and marks it cancelled. It returns the channel the
// transaction's poll loop closes when it stops, so the caller can wait for that
// loop to finish before answering the request.
func (r *deviceLoginRegistry) cancel(id, message string) (<-chan struct{}, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	login := r.logins[id]
	if login == nil {
		return nil, false
	}
	delete(r.logins, id)
	finishDeviceLogin(login, "cancelled", message, 0)
	return login.done, true
}

// pollable returns a copy of the transaction while it is still pollable. The copy
// is deliberate: the poll loop reads several fields after the lock is released,
// and a shared pointer would let a concurrent cancel mutate them underneath it.
func (r *deviceLoginRegistry) pollable(id string) *deviceLogin {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	login := r.logins[id]
	if login == nil || login.status != "pending" || strings.TrimSpace(login.challenge.DeviceCode) == "" {
		return nil
	}
	snapshot := *login
	return &snapshot
}

func (r *deviceLoginRegistry) awaitPoll(ctx context.Context, id, expiredMessage string) *deviceLogin {
	login := r.pollable(id)
	if login == nil {
		return nil
	}
	if time.Now().After(login.expiresAt) {
		r.finish(id, "expired", expiredMessage, 0)
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case <-time.After(login.interval):
		return login
	}
}

// pending reports whether the transaction is still awaiting the browser step.
func (r *deviceLoginRegistry) pending(id string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	login := r.logins[id]
	return login != nil && login.status == "pending"
}

// update runs fn on the stored transaction under the lock, for channel state that
// is not part of the shared bookkeeping. Grok widens its poll interval here when
// the upstream asks it to slow down.
func (r *deviceLoginRegistry) update(id string, fn func(*deviceLogin)) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if login := r.logins[id]; login != nil {
		fn(login)
	}
}

// finish records a transaction's outcome without removing it, so the console can
// still read the result.
func (r *deviceLoginRegistry) finish(id, status, message string, accountID int64) {
	r.update(id, func(login *deviceLogin) {
		finishDeviceLogin(login, status, message, accountID)
	})
}

// cleanup expires timed-out transactions and drops entries that are long past
// their expiry, so a stalled flow cannot hold a slot of the admission limit.
func (r *deviceLoginRegistry) cleanup(now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, login := range r.logins {
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
