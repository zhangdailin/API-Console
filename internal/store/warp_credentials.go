package store

import (
	"context"
	"fmt"
	"time"
)

// WarpCredentialPatch owns only the rotating login credentials. Empty values keep
// stored fields; ExpectedRefreshToken guards against a stale refresh response.
type WarpCredentialPatch struct {
	ExpectedRefreshToken string
	AccessToken          string
	RefreshToken         string
	ExpiresAt            time.Time
}

// UpdateWarpCredentials persists a rotated Warp credential pair atomically without
// replacing concurrent admin, quota, status, or catalog changes.
func (s *Store) UpdateWarpCredentials(ctx context.Context, id int64, patch WarpCredentialPatch) error {
	if s == nil || s.client == nil {
		return fmt.Errorf("store not configured")
	}
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		return applyCredentialPatch("warp", patch.ExpectedRefreshToken, patch.AccessToken, patch.RefreshToken, patch.ExpiresAt,
			&acc.WarpAccessToken, &acc.WarpRefreshToken, &acc.WarpExpiresAt)
	})
}
