package store

import (
	"context"
	"log/slog"
	"strings"
)

// ChangeEmitter receives one notification per persisted account mutation. The
// store stays unaware of the subscribers behind it.
type ChangeEmitter interface {
	Publish(change AccountChange)
}

// AccountChange is the store's own description of a mutation. It is defined here
// (rather than imported) so the store keeps no dependency on the notification
// package; the bus adapts it. Current is left for the emitter to resolve, so the
// store never blocks a write on a subscriber.
type AccountChange struct {
	AccountID int64
	Previous  *Account
	Origin    string
}

const AccountChangeOriginScheduler = "token_refresh_scheduler"

type accountChangeOriginContextKey struct{}

// WithAccountChangeOrigin marks writes made by an internal controller. Cache
// invalidation still happens, while a filtered scheduler subscriber can ignore
// its own persisted result.
func WithAccountChangeOrigin(ctx context.Context, origin string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, accountChangeOriginContextKey{}, strings.TrimSpace(origin))
}

func accountChangeOrigin(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	origin, _ := ctx.Value(accountChangeOriginContextKey{}).(string)
	return strings.TrimSpace(origin)
}

// SetChangeEmitter wires the notification target. Passing nil disables it.
func (s *Store) SetChangeEmitter(emitter ChangeEmitter) {
	if s == nil || s.client == nil {
		return
	}
	s.changeMu.Lock()
	s.changeEmitter = emitter
	s.changeMu.Unlock()
}

// publishChange coalesces pending mutations by account and wakes one dispatcher.
// This preserves the write path's non-blocking contract without creating one
// goroutine per API request. Keeping the earliest Previous value means a burst of
// writes is classified against the state before the burst, which is sufficient
// for every cache/status subscriber while bounding memory by account count.
func (s *Store) publishChange(ctx context.Context, previous *Account, id int64) {
	if s == nil || id == 0 {
		return
	}
	var previousCopy *Account
	if previous != nil {
		copied := *previous
		previousCopy = &copied
	}
	change := AccountChange{AccountID: id, Previous: previousCopy, Origin: accountChangeOrigin(ctx)}

	s.changeMu.Lock()
	if s.changeEmitter == nil || s.changePending == nil || s.changeClosed {
		s.changeMu.Unlock()
		return
	}
	if existing, ok := s.changePending[id]; ok {
		// Preserve the state before the first mutation in the coalesced burst, but
		// keep the newest origin for scheduler feedback suppression.
		existing.Origin = change.Origin
		s.changePending[id] = existing
	} else {
		s.changePending[id] = change
	}
	s.changeMu.Unlock()
	select {
	case s.changeWake <- struct{}{}:
	default:
	}
}

func (s *Store) dispatchChanges() {
	defer close(s.changeDone)
	for {
		select {
		case <-s.changeWake:
			s.flushChanges()
		case <-s.changeStop:
			s.flushChanges()
			return
		}
	}
}

func (s *Store) flushChanges() {
	for {
		s.changeMu.Lock()
		if len(s.changePending) == 0 {
			s.changeMu.Unlock()
			return
		}
		var change AccountChange
		for id, pending := range s.changePending {
			change = pending
			delete(s.changePending, id)
			break
		}
		emitter := s.changeEmitter
		s.changeMu.Unlock()
		if emitter == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("Account change emitter panicked", "error", r)
				}
			}()
			emitter.Publish(change)
		}()
	}
}
