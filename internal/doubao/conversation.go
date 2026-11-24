package doubao

import (
	"container/list"
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// conversationFor opens a child of the account's root. Only a named, bound
// scope can reuse a child; anonymous requests always get a fresh one.
func (c *Client) conversationFor(ctx context.Context, jar cookieJar) (string, error) {
	if c.dialogIdentity == nil {
		return "", errors.New("Doubao conversation identity is not bound")
	}
	key := c.dialogIdentity.scope
	if key == "" || c.acc.ID == 0 {
		root, err := c.mainConversation(ctx, jar)
		if err != nil {
			return "", err
		}
		return c.createScopedConversation(ctx, jar, root, "")
	}
	if cached := childConversations.get(key, time.Now()); cached != "" {
		return cached, nil
	}
	value, err, _ := conversationCreates.Do(key, func() (any, error) {
		if cached := childConversations.get(key, time.Now()); cached != "" {
			return cached, nil
		}
		root, err := c.mainConversation(ctx, jar)
		if err != nil {
			return "", err
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
		childConversations.put(key, id, time.Now())
	}
	return id, nil
}

// mainConversation returns the account's root conversation, creating it once.
func (c *Client) mainConversation(ctx context.Context, jar cookieJar) (string, error) {
	if c.acc.ID != 0 {
		root := rootConversations.get(c.acc.ID, time.Now())
		if root != "" {
			return root, nil
		}
	}
	root, err := c.createConversation(ctx, jar, "")
	if err != nil {
		return "", err
	}
	if c.acc.ID != 0 {
		rootConversations.put(c.acc.ID, root, time.Now())
	}
	return root, nil
}

const conversationCacheTTL = 24 * time.Hour

var (
	rootConversations  = conversationCache[int64]{limit: 1024, ttl: conversationCacheTTL}
	childConversations = conversationCache[string]{limit: 8192, ttl: conversationCacheTTL}
)

type cachedConversation[K comparable] struct {
	key     K
	id      string
	expires time.Time
}

type conversationCache[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*list.Element
	recent  list.List
	limit   int
	ttl     time.Duration
}

func (c *conversationCache[K]) remove(element *list.Element) {
	delete(c.entries, element.Value.(cachedConversation[K]).key)
	c.recent.Remove(element)
}

func (c *conversationCache[K]) prune(now time.Time) {
	for oldest := c.recent.Back(); oldest != nil && !oldest.Value.(cachedConversation[K]).expires.After(now); oldest = c.recent.Back() {
		c.remove(oldest)
	}
}

func (c *conversationCache[K]) get(key K, now time.Time) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	element := c.entries[key]
	if element == nil {
		return ""
	}
	item := element.Value.(cachedConversation[K])
	item.expires = now.Add(c.ttl)
	element.Value = item
	c.recent.MoveToFront(element)
	return item.id
}

func (c *conversationCache[K]) put(key K, id string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	if c.entries == nil {
		c.entries = make(map[K]*list.Element)
	}
	if previous := c.entries[key]; previous != nil {
		c.remove(previous)
	}
	c.entries[key] = c.recent.PushFront(cachedConversation[K]{key: key, id: id, expires: now.Add(c.ttl)})
	if len(c.entries) > c.limit {
		c.remove(c.recent.Back())
	}
}
