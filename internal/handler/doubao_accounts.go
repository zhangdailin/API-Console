package handler

import (
	"context"
	"errors"
	"strings"

	"orchids-api/internal/middleware"
	"orchids-api/internal/store"
)

func (h *Handler) acquireInitialAccount(ctx context.Context, channel, model, dialog string) (UpstreamClient, *store.Account, func(), func(), error) {
	owner := middleware.APIKeyFingerprint(ctx)
	if channel != "doubao" || dialog == "" || owner == "" || h.loadBalancer == nil || h.loadBalancer.Store == nil {
		return h.acquireReservedAccountSelection(ctx, channel, true, nil, model)
	}
	s := h.loadBalancer.Store
	pinned, err := s.DoubaoConversationAccount(ctx, owner, dialog)
	if err != nil {
		return nil, nil, func() {}, nil, err
	}
	acquire := func(id int64) (UpstreamClient, *store.Account, func(), func(), error) {
		return h.acquireReservedAccountSelection(context.WithValue(ctx, pinnedDoubaoAccountKey{}, id), channel, true, nil, model)
	}
	client, account, release, tracked, err := acquire(pinned)
	if err != nil {
		return client, account, release, tracked, err
	}
	if account == nil || account.ID <= 0 {
		release()
		h.releaseTrackedAccount(tracked)
		return nil, nil, func() {}, nil, errors.New("Doubao conversation has no account to bind")
	}
	chosen, err := s.BindDoubaoConversation(ctx, owner, dialog, account.ID)
	if err == nil && chosen == account.ID {
		return client, account, release, tracked, nil
	}
	release()
	h.releaseTrackedAccount(tracked)
	if err != nil {
		return nil, nil, func() {}, nil, err
	}
	// A concurrent first turn may have won the binding on another replica.
	// Reserve its account before any upstream operation for this turn.
	return acquire(chosen)
}

// AcquireDoubaoMediaAccount uses the same pool and per-account reservation as
// chat; it returns a release function that must outlive media task polling.
func (h *Handler) AcquireDoubaoMediaAccount(ctx context.Context, model string, excluded []int64) (*store.Account, func(), error) {
	_, account, release, tracked, err := h.acquireReservedAccountSelection(ctx, "doubao", true, excluded, model)
	if err != nil {
		release()
		h.releaseTrackedAccount(tracked)
		if model == "doubao-video" && strings.Contains(err.Error(), "cooling down for the requested model") {
			return nil, nil, store.ErrDoubaoVideoCooldown
		}
		return nil, nil, err
	}
	if account == nil || strings.TrimSpace(account.ClientCookie) == "" {
		release()
		h.releaseTrackedAccount(tracked)
		return nil, nil, errors.New("Doubao account has no session")
	}
	return account, func() { release(); h.releaseTrackedAccount(tracked) }, nil
}
