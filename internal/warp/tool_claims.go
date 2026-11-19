package warp

import (
	"context"
	"fmt"
	"time"
)

// claimToolTurn atomically consumes the previous response's result set and
// reserves the next paid turn. A failed generation is not retried: the claim is
// deliberately not released. Production Redis clients implement the atomic path;
// local isolated tests use the binding-store mutex.
func (b *bindingStore) claimToolTurn(ctx context.Context, accountID int64, model string, ids []string, binding callBinding) error {
	if b == nil || len(ids) == 0 || binding.ToolRounds < 0 || binding.ConversationID == "" {
		return fmt.Errorf("Warp tool continuation unavailable")
	}
	owner, ok := bindingOwner(ctx)
	if !ok {
		return fmt.Errorf("Warp tool continuation requires authenticated ownership")
	}
	base := owner + "\x00" + fmt.Sprint(accountID) + "\x00" + model
	conversationKey := b.keyBase + "warp:turn:" + hashID(base+"\x00"+binding.ConversationID)
	claims := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			return fmt.Errorf("Warp tool result is missing or duplicated")
		}
		seen[id] = true
		claims = append(claims, b.keyBase+"warp:consumed:"+hashID(base+"\x00"+id))
	}
	if backend, ok := b.redis.(interface {
		ClaimToolTurn(context.Context, string, []string, time.Duration) error
	}); ok {
		return backend.ClaimToolTurn(ctx, conversationKey, claims, bindingTTL)
	}
	if b.redis != nil {
		return fmt.Errorf("Warp continuation claim store unavailable")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claims == nil {
		b.claims = make(map[string]time.Time)
	}
	now := time.Now()
	for key, until := range b.claims {
		if !until.After(now) {
			delete(b.claims, key)
		}
	}
	for _, key := range claims {
		if b.claims[key].After(now) {
			return fmt.Errorf("Warp tool result already consumed")
		}
	}
	if len(b.claims)+len(claims) > bindingLimit {
		return fmt.Errorf("Warp continuation claim capacity reached")
	}
	for _, key := range claims {
		b.claims[key] = now.Add(bindingTTL)
	}
	return nil
}
