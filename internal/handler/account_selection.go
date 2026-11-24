package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/provider"
	"orchids-api/internal/store"
	"strings"
	"time"
)

// acquireAccountSelection is the form the request path uses: it returns the
// release handle for the account client, so a client evicted mid-request is
// closed only after that request finishes.
func (h *Handler) acquireAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, modelID string) (UpstreamClient, *store.Account, func(), error) {
	if h.loadBalancer != nil {
		if targetChannel != "" {
			slog.Debug("Account channel selection", "channel", targetChannel, "channel_required", channelRequired)
		}
		account, err := h.selectAccountRecord(ctx, targetChannel, failedAccountIDs, modelID)
		if err != nil {
			if channelRequired {
				return nil, nil, func() {}, err
			}
			if h.client != nil {
				slog.Debug("Load balancer: no available accounts for channel, using default config", "channel", targetChannel)
				return h.client, nil, func() {}, nil
			}
			return nil, nil, func() {}, err
		}
		client, release := h.acquireAccountClient(account)
		if client == nil {
			return nil, nil, func() {}, errors.New("no client configured")
		}
		return client, account, release, nil
	} else if h.client != nil {
		return h.client, nil, func() {}, nil
	}
	return nil, nil, func() {}, errors.New("no client configured")
}

// acquireReservedAccountSelection closes the check-then-increment race between
// account selection and connection tracking. A candidate is not returned until
// its per-account slot has been atomically reserved; if another request wins the
// last slot, selection continues with the remaining accounts.
func (h *Handler) acquireReservedAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, modelID string) (UpstreamClient, *store.Account, func(), func(), error) {
	excluded := append([]int64(nil), failedAccountIDs...)
	full := make(map[int64]struct{})
	waitBudget := h.configSnapshot().ReservationWaitBudget()
	var waited time.Duration
	for {
		client, account, release, err := h.acquireAccountSelection(ctx, targetChannel, channelRequired, excluded, modelID)
		if err != nil {
			if waited < waitBudget && strings.Contains(err.Error(), "all matching accounts are at their concurrency limit") {
				// Rebuild the exclusion set from caller-supplied failures. Entries in
				// full only represent accounts that lost a reservation race and may
				// be available again on the next pass.
				excluded = append([]int64(nil), failedAccountIDs...)
				clear(full)
				delay := min(250*time.Millisecond, waitBudget-waited)
				waited += delay
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return nil, nil, func() {}, nil, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if len(full) > 0 {
				return nil, nil, func() {}, nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
			}
			return nil, nil, func() {}, nil, err
		}
		releaseSlot, acquired := h.tryAcquireTrackedAccount(ctx, account)
		if acquired {
			return client, account, release, releaseSlot, nil
		}
		release()
		if account == nil || account.ID == 0 {
			return nil, nil, func() {}, nil, fmt.Errorf("failed to reserve an account concurrency slot")
		}
		if _, seen := full[account.ID]; seen {
			return nil, nil, func() {}, nil, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
		}
		full[account.ID] = struct{}{}
		excluded = append(excluded, account.ID)
	}
}

// honorsModelCooldown reports whether a channel's selection consults the
// per-model cooldown its own verdicts write. WorkBuddy may scope a plan refusal
// while accounts with a truly exhausted
// package remain account-wide parked. The answer now comes from the channel's
// registered capabilities instead of a hardcoded list here.
func honorsModelCooldown(channel string) bool {
	return provider.HonorsModelCooldown(channel)
}

type pinnedDoubaoAccountKey struct{}

func (h *Handler) selectAccountRecord(ctx context.Context, targetChannel string, failedAccountIDs []int64, modelID string) (*store.Account, error) {
	if h == nil || h.loadBalancer == nil {
		return nil, errors.New("load balancer not configured")
	}
	model := strings.TrimSpace(modelID)
	channel := strings.ToLower(strings.TrimSpace(targetChannel))
	spec, _ := provider.Lookup(channel)
	pinned, _ := ctx.Value(pinnedDoubaoAccountKey{}).(int64)
	needsFilter := (channel == "doubao" && pinned != 0) || model != "" && (honorsModelCooldown(channel) || channel == "cline" || spec.Selection != nil)
	var filter loadbalancer.AccountFilter
	if needsFilter {
		filter = func(acc *store.Account) error {
			if channel == "doubao" && pinned != 0 && acc.ID != pinned {
				return loadbalancer.ErrAccountNotEligible
			}
			if spec.Selection != nil && spec.Selection.NeedsRefresh != nil && spec.Selection.NeedsRefresh(acc) {
				return loadbalancer.ErrAccountNotEligible
			}
			if !provider.SupportsModel(channel, acc, model) {
				return loadbalancer.ErrModelUnavailable
			}
			if honorsModelCooldown(channel) {
				// The verdict that recorded the cooldown travels with it, so an
				// emptied pool can say whether waiting could ever help.
				switch store.ModelCooldownKind(acc, model, time.Now()) {
				case store.ModelCooldownUnavailable:
					return loadbalancer.ErrModelUnavailable
				case store.ModelCooldownThrottled:
					return loadbalancer.ErrModelThrottled
				}
			}
			switch strings.TrimSpace(acc.StatusCode) {
			case "402", store.AccountStatusWorkBuddyQuotaExhausted:
				if channel == "workbuddy" && provider.IsFreeModel("workbuddy", acc, model) {
					return nil
				}
				return loadbalancer.ErrAccountNotEligible
			default:
				return nil
			}
		}
	}
	selectFresh := func(selectionCtx context.Context) (*store.Account, error) {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(selectionCtx, failedAccountIDs, targetChannel, h.connTracker, filter)
	}
	account, originalErr := selectFresh(ctx)
	if originalErr == nil || model == "" || spec.Selection == nil || spec.Selection.Refresh == nil || spec.Selection.NeedsRefresh == nil || h.loadBalancer.Store == nil {
		return account, originalErr
	}
	// Repair only when the fresh pool cannot serve the request. Network reads
	// stay outside pool locks and before any generation-attempt budget is used.
	refreshCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	excluded := append([]int64(nil), failedAccountIDs...)
	refreshFailed := false
	for range 3 {
		candidate, err := h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(refreshCtx, excluded, targetChannel, h.connTracker, func(acc *store.Account) error {
			if !spec.Selection.NeedsRefresh(acc) {
				return loadbalancer.ErrAccountNotEligible
			}
			return nil
		})
		if err != nil {
			break
		}
		excluded = append(excluded, candidate.ID)
		if err := spec.Selection.Refresh(refreshCtx, provider.Env{Config: h.configSnapshot(), Store: h.loadBalancer.Store}, candidate); err != nil {
			refreshFailed = true
			slog.Warn("Account selection catalog refresh failed", "channel", channel, "account_id", candidate.ID)
		}
		h.loadBalancer.InvalidateAccountSnapshot(candidate)
		if account, err := selectFresh(refreshCtx); err == nil {
			return account, nil
		} else {
			originalErr = err
		}
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if refreshFailed || refreshCtx.Err() != nil {
		return nil, fmt.Errorf("no enabled accounts available for channel: %s (catalog refresh temporarily unavailable)", channel)
	}
	return nil, originalErr
}

func effectiveAccountConcurrencyLimit(acc *store.Account) int64 {
	return loadbalancer.EffectiveAccountConcurrencyLimit(acc)
}

func (h *Handler) tryAcquireTrackedAccount(ctx context.Context, acc *store.Account) (func(), bool) {
	if acc == nil || acc.ID == 0 {
		return nil, true
	}
	limit := effectiveAccountConcurrencyLimit(acc)
	if h != nil && h.connTracker != nil {
		return loadbalancer.ReserveConnection(ctx, h.connTracker, acc.ID, limit)
	}
	if h != nil && h.loadBalancer != nil {
		return h.loadBalancer.ReserveConnection(ctx, acc.ID, limit)
	}
	return nil, true
}

func (h *Handler) releaseTrackedAccount(release func()) {
	if release != nil {
		release()
	}
}
