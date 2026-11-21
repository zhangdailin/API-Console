package grokbase

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"orchids-api/internal/store"
)

func TestRetryAccountSwitchReleasesPreviousSlotAndEnforcesLimit(t *testing.T) {
	type session struct {
		account  *store.Account
		released bool
	}
	current := &session{account: &store.Account{ID: 1}}
	refused := errors.New("upstream refused")
	var calls, marked, audited []int64
	var released []int64
	rebuilds := 0
	_, err := RetryAccountSwitch(t.Context(), current, AccountSwitch[session]{
		Limit:   2,
		Account: func() *store.Account { return current.account },
		Release: func() {
			released = append(released, current.account.ID)
			current.released = true
		},
		Call: func() (*http.Response, error) {
			if current.released || (current.account.ID == 2 && rebuilds != 1) {
				t.Fatal("called without the current reservation and payload")
			}
			calls = append(calls, current.account.ID)
			return nil, refused
		},
		Next: func(used []int64) (*session, error) {
			if !slices.Equal(used, []int64{1}) || !current.released {
				t.Fatal("previous account was not excluded or released")
			}
			return &session{account: &store.Account{ID: 2}}, nil
		},
		Rebuild: func() error {
			if current.account.ID != 2 {
				t.Fatal("rebuild ran before adopting the next reservation")
			}
			rebuilds++
			return nil
		},
		Audit: func(a Attempt) {
			if a.Number != len(calls) || !errors.Is(a.Err, refused) {
				t.Fatal("audit lost attempt number or failure")
			}
			audited = append(audited, a.Account.ID)
		},
		MarkFailure:  func(acc *store.Account, _ error) { marked = append(marked, acc.ID) },
		ShouldSwitch: func(error) bool { return true },
		Wait: func(error) error {
			if !current.released {
				t.Fatal("held the previous account slot while waiting")
			}
			return nil
		},
	})
	want := []int64{1, 2}
	if !errors.Is(err, refused) || !slices.Equal(calls, want) || !slices.Equal(audited, want) || !slices.Equal(marked, want) {
		t.Fatalf("err=%v calls=%v audited=%v marked=%v", err, calls, audited, marked)
	}
	if !slices.Equal(released, []int64{1}) || current.released {
		t.Fatalf("the final reservation must remain with the caller: released=%v", released)
	}
}

func TestRetryAccountSwitchCancellationLeavesReservationToCaller(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cause := errors.New("client ended the request")
	var current struct{}
	refused := errors.New("independent upstream failure")
	audited := false
	_, err := RetryAccountSwitch(ctx, &current, AccountSwitch[struct{}]{
		Limit: 2, CancelEarly: true,
		Account: func() *store.Account { return nil },
		Call: func() (*http.Response, error) {
			cancel(cause)
			return nil, refused
		},
		Audit: func(Attempt) { audited = true },
		MarkFailure: func(*store.Account, error) {
			t.Fatal("cancellation stamped an upstream account verdict")
		},
		Release: func() { t.Fatal("cancellation released the caller's reservation") },
	})
	if !errors.Is(err, cause) || !audited {
		t.Fatalf("err=%v audited=%t", err, audited)
	}
}
