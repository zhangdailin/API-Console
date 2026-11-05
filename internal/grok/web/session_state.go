package web

import (
	"context"

	"orchids-api/internal/store"
)

// The session's account binding lives in grokbase.Affinity; these pass it the
// plane's store.
func (h *Handler) affinityAccount(ctx context.Context, provider string) int64 {
	if h == nil {
		return 0
	}
	return h.affinity.Account(ctx, h.affinityStore(), provider)
}

func (h *Handler) bindAffinity(ctx context.Context, provider string, accountID int64) {
	if h == nil {
		return
	}
	h.affinity.Bind(ctx, h.affinityStore(), provider, accountID)
}

func (h *Handler) unbindAffinity(ctx context.Context, provider string, accountID int64) {
	if h == nil {
		return
	}
	h.affinity.Unbind(ctx, h.affinityStore(), provider, accountID)
}

func (h *Handler) affinityStore() *store.Store {
	if h.lb == nil {
		return nil
	}
	return h.lb.Store
}
