package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// WarpAuthFailurePatch records a terminal Firebase verdict only for the exact
// credential generation that was rejected, never a concurrent replacement.
type WarpAuthFailurePatch struct {
	ExpectedRefreshToken string
	ExpectedAccessToken  string
	ExpectedExpiresAt    time.Time
	Message              string
	ObservedAt           time.Time
}

func (s *Store) UpdateWarpAuthFailure(ctx context.Context, id int64, patch WarpAuthFailurePatch) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("warp auth state store not configured")
	}
	if patch.ExpectedRefreshToken == "" || patch.ObservedAt.IsZero() {
		return fmt.Errorf("invalid warp auth failure patch")
	}
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if !strings.EqualFold(acc.AccountType, "warp") || acc.WarpRefreshToken != patch.ExpectedRefreshToken ||
			acc.WarpAccessToken != patch.ExpectedAccessToken || !acc.WarpExpiresAt.Equal(patch.ExpectedExpiresAt) {
			return fmt.Errorf("warp credential changed concurrently")
		}
		acc.AuthStatus = AccountAuthStatusReauthRequired
		acc.StatusCode, acc.StatusMessage = "401", patch.Message
		acc.VerifiedAt, acc.LastAttempt = patch.ObservedAt, patch.ObservedAt
		return nil
	})
}
