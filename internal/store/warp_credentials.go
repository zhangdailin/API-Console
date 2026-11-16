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
	if s != nil && s.accounts != nil {
		return s.accounts.UpdateWarpCredentials(ctx, id, patch)
	}
	return fmt.Errorf("store not configured")
}

func (s *redisStore) UpdateWarpCredentials(ctx context.Context, id int64, patch WarpCredentialPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		return applyCredentialPatch("warp", patch.ExpectedRefreshToken, patch.AccessToken, patch.RefreshToken, patch.ExpiresAt,
			&acc.WarpAccessToken, &acc.WarpRefreshToken, &acc.WarpExpiresAt)
	})
}

// UpdateWarpCatalog persists only a newer catalog observed with the expected
// credential, preserving concurrent operator, credential, status, and usage edits.
func (s *Store) UpdateWarpCatalog(ctx context.Context, id int64, expectedRefresh string, ids []string, syncedAt time.Time) error {
	if s != nil && s.accounts != nil {
		if backend, ok := s.accounts.(interface {
			UpdateWarpCatalog(context.Context, int64, string, []string, time.Time) error
		}); ok {
			return backend.UpdateWarpCatalog(ctx, id, expectedRefresh, ids, syncedAt)
		}
	}
	return fmt.Errorf("warp catalog store not configured")
}

func (s *redisStore) UpdateWarpCatalog(ctx context.Context, id int64, expectedRefresh string, ids []string, syncedAt time.Time) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if acc.WarpRefreshToken != expectedRefresh {
			return fmt.Errorf("warp credential changed concurrently")
		}
		if !syncedAt.After(acc.WarpModelsSyncedAt) {
			return nil
		}
		acc.WarpModelIDs = append([]string(nil), ids...)
		acc.WarpModelsSyncedAt = syncedAt
		return nil
	})
}
