package grok

import (
	"context"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// baseHandler tracks account connections and records account verdicts for the
// native handler. Its methods are safe on a nil receiver, so a Handler built
// without one simply tracks nothing.
type baseHandler struct {
	LB *loadbalancer.LoadBalancer
}

// TrackAccount acquires a connection slot for the account and returns
// a release function. Safe to call with nil account.
func (b *baseHandler) TrackAccount(acc *store.Account) func() {
	if b == nil || b.LB == nil || acc == nil || acc.ID == 0 {
		return func() {}
	}
	b.LB.AcquireConnection(acc.ID)
	return func() { b.LB.ReleaseConnection(acc.ID) }
}

// MarkAccountStatus classifies an error string and marks the account
// status in the store if it indicates a known HTTP error (401/403/404/429).
func (b *baseHandler) MarkAccountStatus(ctx context.Context, acc *store.Account, err error) {
	if acc == nil || err == nil || b == nil || b.LB == nil {
		return
	}
	status := apperrors.ClassifyAccountStatus(err.Error())
	if status == "" {
		return
	}
	b.LB.MarkAccountStatus(ctx, acc, status)
}

func newBaseHandler(lb *loadbalancer.LoadBalancer) *baseHandler { return &baseHandler{LB: lb} }
