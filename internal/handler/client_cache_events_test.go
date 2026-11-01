package handler

import (
	"context"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/debug"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
	"orchids-api/internal/upstream"
)

// fakeCachedClient records whether it was closed, so a test can assert when the
// old client is released.
type fakeCachedClient struct {
	name   string
	mu     sync.Mutex
	closed bool
}

func (c *fakeCachedClient) SendRequestWithPayload(_ context.Context, _ upstream.UpstreamRequest, _ func(upstream.SSEMessage), _ *debug.Logger) error {
	return nil
}

func (c *fakeCachedClient) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func (c *fakeCachedClient) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// cacheTestHandler builds a handler whose client factory hands out distinguishable
// clients and counts how many were built.
type cacheTestHandler struct {
	handler *Handler
	mu      sync.Mutex
	built   []*fakeCachedClient
	// current is the state the resolver reports, i.e. what was persisted.
	current map[int64]*store.Account
}

func newCacheTestHandler(t *testing.T) *cacheTestHandler {
	t.Helper()
	harness := &cacheTestHandler{}
	h := &Handler{
		config:      &config.Config{},
		clientCache: newAccountClientCache(),
	}
	h.clientFactory = func(_ *store.Account, _ *config.Config) UpstreamClient {
		harness.mu.Lock()
		defer harness.mu.Unlock()
		client := &fakeCachedClient{name: "client"}
		harness.built = append(harness.built, client)
		return client
	}
	// Wire the cache exactly as production does: same config, same reader.
	h.clientCache.SetConfig(h.config)
	// The cache re-reads the account when told it changed; a test drives that
	// reader with whatever state it wants the cache to observe.
	h.clientCache.SetAccountResolver(func(id int64) *store.Account {
		harness.mu.Lock()
		defer harness.mu.Unlock()
		return harness.current[id]
	})
	harness.current = map[int64]*store.Account{}
	harness.handler = h
	return harness
}

// setCurrent makes the resolver report this state for the account.
func (c *cacheTestHandler) setCurrent(account *store.Account) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if account == nil {
		return
	}
	c.current[account.ID] = account
}

func (c *cacheTestHandler) builtClients() []*fakeCachedClient {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*fakeCachedClient(nil), c.built...)
}

func testAccount(id int64, session string) *store.Account {
	return &store.Account{ID: id, AccountType: "workbuddy", RefreshToken: session, Enabled: true, Weight: 1}
}

// TestAccountChanges_RotationBuildsANewClient is the acceptance rule for
// credential rotation: the next request must not reuse a client built from the
// replaced credential.
func TestAccountChanges_RotationBuildsANewClient(t *testing.T) {
	fixture := newCacheTestHandler(t)
	account := testAccount(11, "session-a")

	first, release := fixture.handler.acquireAccountClient(account)
	testutil.False(t, first == nil, "no client built")
	release()

	// Rotate the credential and notify, as the change bus would.
	rotated := testAccount(11, "session-b")
	fixture.setCurrent(rotated)
	fixture.handler.AccountChanges([]int64{11})

	second, releaseSecond := fixture.handler.acquireAccountClient(rotated)
	defer releaseSecond()
	testutil.NotEqual(t, second, first)
	built := fixture.builtClients()
	testutil.Falsef(t, len(built) != 2, "clients built = %d, want a new one after rotation", len(built))
	// The evicted client was idle, so it is closed immediately.
	testutil.False(t, !fixture.builtClients()[0].isClosed(), "the evicted idle client was not closed")
}

// TestAccountChanges_DoesNotCloseAClientInUse is the concurrency guarantee: a
// credential change must not tear down the connection a running request is using.
func TestAccountChanges_DoesNotCloseAClientInUse(t *testing.T) {
	fixture := newCacheTestHandler(t)
	account := testAccount(12, "session-a")

	inUse, releaseInUse := fixture.handler.acquireAccountClient(account)
	testutil.False(t, inUse == nil, "no client built")
	clientInUse := fixture.builtClients()[0]

	// A rotation arrives while the request is still running.
	rotated := testAccount(12, "session-b")
	fixture.setCurrent(rotated)
	fixture.handler.AccountChanges([]int64{12})
	testutil.False(t, clientInUse.isClosed(), "a client in use was closed by a credential change")

	// A new request already gets the fresh client.
	fresh, releaseFresh := fixture.handler.acquireAccountClient(rotated)
	releaseFresh()
	testutil.NotEqual(t, fresh, inUse)

	// Finishing the first request releases the old client.
	releaseInUse()
	testutil.False(t, !clientInUse.isClosed(), "the retired client was not closed after its last user finished")
}

// TestAccountChanges_DeleteEvictsTheClient covers removal: no client survives for
// an account that no longer exists.
func TestAccountChanges_DeleteEvictsTheClient(t *testing.T) {
	fixture := newCacheTestHandler(t)
	account := testAccount(13, "session-a")

	client, release := fixture.handler.acquireAccountClient(account)
	release()
	testutil.False(t, client == nil, "no client built")

	fixture.handler.AccountChanges([]int64{13})

	fixture.handler.clientCache.mu.RLock()
	_, cached := fixture.handler.clientCache.entries[13]
	fixture.handler.clientCache.mu.RUnlock()
	testutil.False(t, cached, "the client of a deleted account is still cached")
	testutil.False(t, !fixture.builtClients()[0].isClosed(), "the client of a deleted account was not closed")
}

// TestAcquireRelease_IsIdempotent keeps a double release from closing a client a
// later request is still holding.
func TestAcquireRelease_IsIdempotent(t *testing.T) {
	fixture := newCacheTestHandler(t)
	account := testAccount(14, "session-a")

	client, release := fixture.handler.acquireAccountClient(account)
	testutil.False(t, client == nil, "no client built")
	release()
	release()

	second, releaseSecond := fixture.handler.acquireAccountClient(account)
	defer releaseSecond()
	testutil.Equal(t, second, client)
	testutil.False(t, fixture.builtClients()[0].isClosed(), "an extra release closed a client still in the cache")
}

// TestAcquireRelease_UnchangedAccountReusesTheClient pins what must NOT change: a
// status-only update must not force a rebuild, or every refresh cycle would drop
// the keep-alive pool.
func TestAcquireRelease_UnchangedAccountReusesTheClient(t *testing.T) {
	fixture := newCacheTestHandler(t)
	account := testAccount(15, "session-a")

	first, releaseFirst := fixture.handler.acquireAccountClient(account)
	releaseFirst()

	same := testAccount(15, "session-a")
	same.StatusCode = "429"
	same.StatusMessage = "throttled"
	same.VerifiedAt = time.Now()
	fixture.setCurrent(same)
	fixture.handler.AccountChanges([]int64{15})

	second, releaseSecond := fixture.handler.acquireAccountClient(same)
	defer releaseSecond()
	testutil.Equal(t, second, first)
	testutil.Equal(t, len(fixture.builtClients()), 1)
}
