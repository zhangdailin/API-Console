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

type accountSelectionOptions struct {
	ModelID string
}

// acquireAccountSelection is the form the request path uses: it returns the
// release handle for the account client, so a client evicted mid-request is
// closed only after that request finishes.
func (h *Handler) acquireAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, opts accountSelectionOptions) (UpstreamClient, *store.Account, func(), error) {
	if h.loadBalancer != nil {
		if targetChannel != "" {
			slog.Debug("Account channel selection", "channel", targetChannel, "channel_required", channelRequired)
		}
		account, err := h.selectAccountRecordWithOptions(ctx, targetChannel, failedAccountIDs, opts)
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
func (h *Handler) acquireReservedAccountSelection(ctx context.Context, targetChannel string, channelRequired bool, failedAccountIDs []int64, opts accountSelectionOptions) (UpstreamClient, *store.Account, func(), int64, error) {
	excluded := append([]int64(nil), failedAccountIDs...)
	full := make(map[int64]struct{})
	// Account leases are held for the complete upstream request. When another
	// request is just finishing, an immediate second selection can observe all
	// accounts at their hard limit and turn a transient race into a 503. Give
	// releases a bounded, cancellable window to become visible before failing.
	// The two-second window matches the busy retry used by Qoder and is still
	// short enough that a genuinely saturated pool fails promptly.
	const (
		reservationRetries    = 8
		reservationRetryDelay = 250 * time.Millisecond
	)
	reservationAttempt := 0
	for {
		client, account, release, err := h.acquireAccountSelection(ctx, targetChannel, channelRequired, excluded, opts)
		if err != nil {
			if reservationAttempt < reservationRetries && strings.Contains(err.Error(), "all matching accounts are at their concurrency limit") {
				reservationAttempt++
				// Rebuild the exclusion set from caller-supplied failures. Entries in
				// full only represent accounts that lost a reservation race and may
				// be available again on the next pass.
				excluded = append([]int64(nil), failedAccountIDs...)
				clear(full)
				timer := time.NewTimer(reservationRetryDelay)
				select {
				case <-ctx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return nil, nil, func() {}, 0, ctx.Err()
				case <-timer.C:
				}
				continue
			}
			if len(full) > 0 {
				return nil, nil, func() {}, 0, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
			}
			return nil, nil, func() {}, 0, err
		}
		accountID, acquired := h.tryAcquireTrackedAccount(account)
		if acquired {
			return client, account, release, accountID, nil
		}
		release()
		if account == nil || account.ID == 0 {
			return nil, nil, func() {}, 0, fmt.Errorf("failed to reserve an account concurrency slot")
		}
		if _, seen := full[account.ID]; seen {
			return nil, nil, func() {}, 0, fmt.Errorf("no enabled accounts available for channel: %s (all matching accounts are at their concurrency limit)", targetChannel)
		}
		full[account.ID] = struct{}{}
		excluded = append(excluded, account.ID)
	}
}

// honorsModelCooldown reports whether a channel's selection consults the
// per-model cooldown its own verdicts write. Qoder scopes agent/model windows;
// WorkBuddy may scope a plan refusal while accounts with a truly exhausted
// package remain account-wide parked. The answer now comes from the channel's
// registered capabilities instead of a hardcoded list here.
func honorsModelCooldown(channel string) bool {
	return provider.HonorsModelCooldown(channel)
}

func (h *Handler) isCurrentFreeModel(ctx context.Context, channel, modelID string) bool {
	if h == nil || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return false
	}
	model, err := h.loadBalancer.Store.GetModelByChannelAndModelID(ctx, channel, strings.ToLower(strings.TrimSpace(modelID)))
	return err == nil && model != nil && strings.EqualFold(strings.TrimSpace(model.BillingTier), "free")
}

func (h *Handler) selectAccountRecordWithOptions(ctx context.Context, targetChannel string, failedAccountIDs []int64, opts accountSelectionOptions) (*store.Account, error) {
	if h == nil || h.loadBalancer == nil {
		return nil, errors.New("load balancer not configured")
	}
	model := strings.TrimSpace(opts.ModelID)
	channel := strings.ToLower(strings.TrimSpace(targetChannel))
	// honorsModelCooldown already covers the qoder and workbuddy channels; cline
	// is the one extra channel whose catalog the filter below understands.
	needsFilter := model != "" && (honorsModelCooldown(channel) || channel == "cline")
	if needsFilter {
		return h.loadBalancer.GetNextAccountExcludingByChannelWithTrackerFilter(ctx, failedAccountIDs, targetChannel, h.connTracker, func(acc *store.Account) error {
			if channel == "cline" && !provider.SupportsModel("cline", acc, model) {
				return loadbalancer.ErrAccountNotEligible
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
			case "402":
				if channel == "qoder" {
					if provider.IsFreeModel("qoder", acc, model) && h.isCurrentFreeModel(ctx, "qoder", model) {
						return nil
					}
					return loadbalancer.ErrAccountNotEligible
				}
				if channel == "workbuddy" {
					if provider.IsFreeModel("workbuddy", acc, model) {
						return nil
					}
					return loadbalancer.ErrAccountNotEligible
				}
			case store.AccountStatusQoderQuotaExhausted:
				if channel == "qoder" && provider.IsFreeModel("qoder", acc, model) && h.isCurrentFreeModel(ctx, "qoder", model) {
					return nil
				}
				return loadbalancer.ErrAccountNotEligible
			case store.AccountStatusWorkBuddyQuotaExhausted:
				if channel == "workbuddy" && provider.IsFreeModel("workbuddy", acc, model) {
					return nil
				}
				return loadbalancer.ErrAccountNotEligible
			default:
				return nil
			}
			return loadbalancer.ErrAccountNotEligible
		})
	}
	return h.loadBalancer.GetNextAccountExcludingByChannelWithTracker(ctx, failedAccountIDs, targetChannel, h.connTracker)
}

func effectiveAccountConcurrencyLimit(acc *store.Account) int64 {
	return loadbalancer.EffectiveAccountConcurrencyLimit(acc)
}

func (h *Handler) tryAcquireTrackedAccount(acc *store.Account) (int64, bool) {
	if acc == nil || acc.ID == 0 {
		return 0, true
	}
	limit := effectiveAccountConcurrencyLimit(acc)
	if h != nil && h.connTracker != nil {
		if limited, ok := h.connTracker.(loadbalancer.LimitedConnTracker); ok && limit > 0 {
			if !limited.TryAcquire(acc.ID, limit) {
				return 0, false
			}
			return acc.ID, true
		}
		if limit > 0 && h.connTracker.GetCount(acc.ID) >= limit {
			return 0, false
		}
		h.connTracker.Acquire(acc.ID)
		return acc.ID, true
	}
	if h != nil && h.loadBalancer != nil {
		// The load balancer's built-in tracker is only a compatibility fallback;
		// production wires a shared tracker directly onto the handler.
		h.loadBalancer.AcquireConnection(acc.ID)
		return acc.ID, true
	}
	return 0, true
}

func (h *Handler) releaseTrackedAccount(accountID int64) {
	if accountID == 0 {
		return
	}
	if h != nil && h.connTracker != nil {
		h.connTracker.Release(accountID)
		return
	}
	if h != nil && h.loadBalancer != nil {
		h.loadBalancer.ReleaseConnection(accountID)
	}
}
