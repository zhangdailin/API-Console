// Package grokbase holds small pieces the Grok Build and Grok Web planes share
// verbatim: account connection tracking and upstream attempt diagnostics.
// Larger shared subsystems have their own packages (egresspool, ratelimit).
package grokbase

import (
	"context"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// AccountTracker tracks account connections and records account verdicts for the
// plane handlers. Its methods are safe on a nil receiver, so a Handler built
// without one simply tracks nothing.
type AccountTracker struct {
	LB *loadbalancer.LoadBalancer
}

// TrackAccount acquires a connection slot for the account and returns
// a release function. Safe to call with nil account.
func (b *AccountTracker) TrackAccount(acc *store.Account) func() {
	if b == nil || b.LB == nil || acc == nil || acc.ID == 0 {
		return func() {}
	}
	b.LB.AcquireConnection(acc.ID)
	return func() { b.LB.ReleaseConnection(acc.ID) }
}

// MarkAccountStatus classifies an error string and marks the account
// status in the store if it indicates a known HTTP error (401/403/404/429).
func (b *AccountTracker) MarkAccountStatus(ctx context.Context, acc *store.Account, err error) {
	if acc == nil || err == nil || b == nil || b.LB == nil {
		return
	}
	status := apperrors.ClassifyAccountStatus(err.Error())
	if status == "" {
		return
	}
	b.LB.MarkAccountStatus(ctx, acc, status)
}

// NewAccountTracker returns a tracker over lb (nil tracks nothing).
func NewAccountTracker(lb *loadbalancer.LoadBalancer) *AccountTracker {
	return &AccountTracker{LB: lb}
}

// AccountCapacityAvailable reports whether acc has a free connection slot on
// tracker. Without a tracker or a limit every account has capacity.
func AccountCapacityAvailable(tracker loadbalancer.ConnTracker, acc *store.Account) bool {
	limit := loadbalancer.EffectiveAccountConcurrencyLimit(acc)
	if acc == nil || limit <= 0 || tracker == nil {
		return true
	}
	return tracker.GetCount(acc.ID) < limit
}

// Reserve takes a connection slot for acc on tracker and returns its release.
// It reports false when the account is at its concurrency limit. Without a
// tracker the slot is taken on the load balancer instead.
func (b *AccountTracker) Reserve(tracker loadbalancer.ConnTracker, acc *store.Account) (func(), bool) {
	if tracker == nil || acc == nil || acc.ID == 0 {
		return b.TrackAccount(acc), true
	}
	if limiter, ok := tracker.(loadbalancer.LimitedConnTracker); ok {
		if !limiter.TryAcquire(acc.ID, loadbalancer.EffectiveAccountConcurrencyLimit(acc)) {
			return func() {}, false
		}
	} else {
		if !AccountCapacityAvailable(tracker, acc) {
			return func() {}, false
		}
		tracker.Acquire(acc.ID)
	}
	return func() { tracker.Release(acc.ID) }, true
}
