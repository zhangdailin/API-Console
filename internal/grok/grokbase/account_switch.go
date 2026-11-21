package grokbase

import (
	"context"
	"net/http"
	"time"

	"orchids-api/internal/store"
)

// AccountSwitch keeps the attempt lifecycle shared while each plane owns its
// account verdicts, replay rules, and wait policy.
type AccountSwitch[S any] struct {
	Limit        int
	CancelEarly  bool
	Account      func() *store.Account
	Release      func()
	Call         func() (*http.Response, error)
	Next         func(used []int64) (*S, error)
	Rebuild      func() error
	Audit        func(Attempt)
	MarkFailure  func(*store.Account, error)
	ShouldSwitch func(error) bool
	AllowSwitch  func(error) bool
	Wait         func(error) error
}

// RetryAccountSwitch leaves the final reservation with the caller to close.
func RetryAccountSwitch[S any](ctx context.Context, session *S, p AccountSwitch[S]) (*http.Response, error) {
	var used []int64
	for attempt := 1; ; attempt++ {
		if p.CancelEarly && ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if acc := p.Account(); acc != nil && acc.ID != 0 {
			used = append(used, acc.ID)
		}
		started := time.Now()
		resp, err := p.Call()
		acc := p.Account()
		p.Audit(Attempt{Account: acc, Number: attempt, Started: started, Err: err, Stage: "account_attempt", Response: resp})
		if err == nil {
			return resp, nil
		}
		if p.CancelEarly && ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		p.MarkFailure(acc, err)
		if !p.ShouldSwitch(err) || attempt >= p.Limit || (p.AllowSwitch != nil && !p.AllowSwitch(err)) {
			return nil, err
		}
		p.Release()
		if waitErr := p.Wait(err); waitErr != nil {
			return nil, waitErr
		}
		next, switchErr := p.Next(used)
		if switchErr != nil {
			return nil, err
		}
		*session = *next
		if p.Rebuild != nil {
			if err := p.Rebuild(); err != nil {
				return nil, err
			}
		}
	}
}
