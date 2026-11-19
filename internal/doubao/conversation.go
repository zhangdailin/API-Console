package doubao

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"golang.org/x/sync/singleflight"
)

// conversationFor returns the upstream conversation that carries one downstream
// conversation, creating it on first use.
//
// Isolation is keyed by the caller's conversation id rather than the account.
// Sharing one upstream conversation per account let one client's history leak
// into another client's answer and grew a single conversation without bound.
// Anonymous bound requests create an uncached child. Only direct legacy helper
// calls without a bound request identity retain their old root-only behavior.
func (c *Client) conversationFor(ctx context.Context, jar cookieJar, downstreamID string) (string, error) {
	account := c.acc.ID
	root, err := c.mainConversation(ctx, jar)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(downstreamID) == "" && c.dialogIdentity == nil {
		// Without a downstream id there is nothing to isolate: keep using the
		// account's root so a turn does not mint a new upstream session.
		return root, nil
	}
	key := cacheKey(account, downstreamID)
	if c.dialogIdentity != nil {
		key = c.dialogIdentity.scope
		if c.dialogIdentity.anonymous {
			key = ""
		}
	}
	if account == 0 {
		key = ""
	}
	if key != "" {
		conversations.Lock()
		cached := conversations.byConversation[key]
		conversations.Unlock()
		if cached != "" {
			return cached, nil
		}
		value, err, _ := conversationCreates.Do(key, func() (any, error) {
			conversations.Lock()
			cached := conversations.byConversation[key]
			conversations.Unlock()
			if cached != "" {
				return cached, nil
			}
			return c.createScopedConversation(ctx, jar, downstreamID, root, key)
		})
		if err != nil {
			return "", err
		}
		return value.(string), nil
	}
	return c.createScopedConversation(ctx, jar, downstreamID, root, key)
}

var conversationCreates singleflight.Group

func (c *Client) createScopedConversation(ctx context.Context, jar cookieJar, downstreamID, root, key string) (string, error) {
	// Creating with a source id opens a *child* conversation. Creating without
	// one returns the account's root and is idempotent, so it can never isolate
	// two downstream conversations.
	id, err := c.createConversation(ctx, jar, root)
	if err != nil {
		return "", err
	}
	slog.Debug("doubao conversation selected", "account_id", c.acc.ID, "root", root, "conversation", id)
	if key != "" {
		conversations.Lock()
		conversations.byConversation[key] = id
		conversations.Unlock()
	}
	return id, nil
}

// mainConversation returns the account's root conversation, creating it once.
func (c *Client) mainConversation(ctx context.Context, jar cookieJar) (string, error) {
	if c.acc.ID != 0 {
		conversations.Lock()
		root := conversations.main[c.acc.ID]
		conversations.Unlock()
		if root != "" {
			return root, nil
		}
	}
	root, err := c.createConversation(ctx, jar, "")
	if err != nil {
		return "", err
	}
	if c.acc.ID != 0 {
		conversations.Lock()
		conversations.main[c.acc.ID] = root
		conversations.Unlock()
	}
	return root, nil
}

// cacheKey scopes a conversation to an account and a downstream conversation.
// An unknown account disables caching rather than sharing one bucket.
func cacheKey(account int64, downstreamID string) string {
	if account == 0 {
		return ""
	}
	return fmt.Sprintf("%d|%s", account, strings.TrimSpace(downstreamID))
}

var conversations = struct {
	sync.Mutex
	main           map[int64]string
	byConversation map[string]string
}{main: map[int64]string{}, byConversation: map[string]string{}}
