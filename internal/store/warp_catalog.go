package store

import (
	"context"
	"fmt"
	"time"
)

// WarpCatalogPatch owns only the observations of one authenticated catalog
// read. ExpectedRefreshToken ties them to the credential generation that
// produced them; budgets and vision flags belong only to this model ID list.
type WarpCatalogPatch struct {
	ExpectedRefreshToken string
	ModelIDs             []string
	ModelWindows         map[string]int
	ModelVision          map[string]bool
	SyncedAt             time.Time
}

// UpdateWarpCatalogSnapshot persists a catalog observation and its per-model
// model metadata atomically, without replaying operator, credential, status,
// quota, or usage state from the account that performed the read. A snapshot
// from a previous credential generation, or one that is not newer than the
// stored catalog, is rejected as a whole: IDs and metadata can never come from
// different observations.
func (s *Store) UpdateWarpCatalogSnapshot(ctx context.Context, id int64, patch WarpCatalogPatch) error {
	if s != nil && s.accounts != nil {
		if backend, ok := s.accounts.(interface {
			UpdateWarpCatalogSnapshot(context.Context, int64, WarpCatalogPatch) error
		}); ok {
			return backend.UpdateWarpCatalogSnapshot(ctx, id, patch)
		}
	}
	return fmt.Errorf("warp catalog store not configured")
}

func (s *redisStore) UpdateWarpCatalogSnapshot(ctx context.Context, id int64, patch WarpCatalogPatch) error {
	return s.updateAccountAtomic(ctx, id, func(acc *Account) error {
		if acc.WarpRefreshToken != patch.ExpectedRefreshToken {
			return fmt.Errorf("warp credential changed concurrently")
		}
		if !patch.SyncedAt.After(acc.WarpModelsSyncedAt) {
			return nil
		}
		acc.WarpModelIDs = append([]string(nil), patch.ModelIDs...)
		acc.WarpModelWindows = cloneWarpModelWindows(patch.ModelWindows)
		acc.WarpModelVision = cloneWarpModelVision(patch.ModelIDs, patch.ModelVision)
		acc.WarpModelsSyncedAt = patch.SyncedAt
		return nil
	})
}

// cloneWarpModelVision keeps only observations for IDs in the same snapshot.
func cloneWarpModelVision(ids []string, vision map[string]bool) map[string]bool {
	if len(vision) == 0 {
		return nil
	}
	var result map[string]bool
	for _, id := range ids {
		if supported, observed := vision[id]; observed {
			if result == nil {
				result = make(map[string]bool, len(vision))
			}
			result[id] = supported
		}
	}
	return result
}

// cloneWarpModelWindows copies only positive windows. A missing or non-positive
// window is unknown, not a guess, and is left out of the observation.
func cloneWarpModelWindows(windows map[string]int) map[string]int {
	if len(windows) == 0 {
		return nil
	}
	cloned := make(map[string]int, len(windows))
	for id, window := range windows {
		if window > 0 {
			cloned[id] = window
		}
	}
	if len(cloned) == 0 {
		return nil
	}
	return cloned
}
