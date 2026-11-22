package doubao

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"golang.org/x/sync/singleflight"
)

// conversationFor opens a child of the account's root. Only a named, bound
// scope can reuse a child; anonymous requests always get a fresh one.
func (c *Client) conversationFor(ctx context.Context, jar cookieJar) (string, error) {
	if c.dialogIdentity == nil {
		return "", errors.New("Doubao conversation identity is not bound")
	}
	root, err := c.mainConversation(ctx, jar)
	if err != nil {
		return "", err
	}
	key := c.dialogIdentity.scope
	if key == "" || c.acc.ID == 0 {
		return c.createScopedConversation(ctx, jar, root, "")
	}
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
		return c.createScopedConversation(ctx, jar, root, key)
	})
	if err != nil {
		return "", err
	}
	return value.(string), nil
}

var conversationCreates singleflight.Group

func (c *Client) createScopedConversation(ctx context.Context, jar cookieJar, root, key string) (string, error) {
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

var conversations = struct {
	sync.Mutex
	main           map[int64]string
	byConversation map[string]string
}{main: map[int64]string{}, byConversation: map[string]string{}}
