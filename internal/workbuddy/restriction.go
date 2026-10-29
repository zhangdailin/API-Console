package workbuddy

import (
	"context"
	"log/slog"
	"time"

	"orchids-api/internal/accountpolicy"
	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

// persistRestriction covers control-plane calls as well as chat, so background
// and manual entrances cannot bypass the routing verdict.
func (c *Client) persistRestriction(ctx context.Context, err error) {
	if c == nil || err == nil || !apperrors.IsWorkBuddyRestricted(err.Error()) {
		return
	}
	c.mu.Lock()
	acc, owner := c.account, c.accountStore
	if acc == nil {
		c.mu.Unlock()
		return
	}
	snapshot := *acc
	acc = &snapshot
	c.mu.Unlock()
	// The response may arrive after cancellation; still save the restriction.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	latest, readErr := refreshqueue.LatestAccount(persistCtx, owner, acc.ID)
	if readErr != nil {
		slog.Warn("Read WorkBuddy restriction state failed", "account_id", acc.ID, "error", readErr)
		return
	}
	if latest != nil {
		acc = latest
	}
	alreadyRestricted := store.WorkBuddyRestrictionUntil(acc).After(time.Now())
	if !alreadyRestricted {
		accountpolicy.Classify(acc, err, "").Apply(acc)
	}
	c.mu.Lock()
	c.account.WorkBuddyRestrictedUntil = store.WorkBuddyRestrictionUntil(acc)
	c.mu.Unlock()
	if alreadyRestricted {
		return
	}
	if writer, ok := owner.(interface {
		UpdateAccount(context.Context, *store.Account) error
	}); ok {
		if saveErr := writer.UpdateAccount(persistCtx, acc); saveErr != nil {
			slog.Warn("Persist WorkBuddy restriction failed", "account_id", acc.ID, "error", saveErr)
		}
	}
}
