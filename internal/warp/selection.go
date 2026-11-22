package warp

import (
	"context"
	"errors"
	"strings"
	"time"

	"orchids-api/internal/provider"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

// Refresh ahead of the hard freshness boundary so a snapshot cannot expire
// between pool selection and the client's final pre-generation check.
func selectionRefreshDue(acc *store.Account) bool {
	return !freshSnapshot(acc) || time.Since(acc.WarpModelsSyncedAt) >= provider.HealthRefreshInterval-time.Minute || !credentials(acc).valid()
}

func refreshSelectionCatalog(ctx context.Context, env provider.Env, snapshot *store.Account) error {
	if env.Store == nil || snapshot == nil || snapshot.ID == 0 {
		return errors.New("Warp catalog refresh requires an account store")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// A separate gate from credential rotation permits the catalog read to
	// refresh credentials internally without re-entering the same lock.
	release, err := refreshqueue.AcquireCredential(ctx, "warp_catalog", env.Store, snapshot.ID)
	if err != nil {
		return err
	}
	defer release()
	current, err := env.Store.GetAccount(ctx, snapshot.ID)
	if err != nil {
		return errors.New("Warp catalog account read failed")
	}
	if current == nil || !strings.EqualFold(current.AccountType, "warp") || !current.Enabled || !store.AccountAuthActive(current) {
		return errors.New("Warp catalog account is no longer eligible")
	}
	if !selectionRefreshDue(current) {
		return nil
	}
	c := newClientWithStore(current, env.Config, env.Store)
	observation, err := c.observeCatalog(ctx)
	if err != nil {
		return err
	}
	if observation.Record == nil || !observation.Record(current, time.Now()) {
		return errors.New("Warp catalog observation was empty")
	}
	return env.Store.UpdateWarpCatalogSnapshot(ctx, current.ID, catalogPatch(current))
}
