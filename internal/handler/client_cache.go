package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"orchids-api/internal/provider"
	"strconv"
	"sync"
	"time"

	"orchids-api/internal/accountevents"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

type cachedAccountClient struct {
	fingerprint string
	client      UpstreamClient
	// inUse counts the requests currently holding this client. An evicted client
	// is only closed once the last holder releases it, so a credential change
	// never tears down a connection a request is still using.
	inUse int
}

// retiredClient is a client that has been evicted but is still in use.
type retiredClient struct {
	client UpstreamClient
	inUse  int
}

type accountClientCache struct {
	mu      sync.RWMutex
	entries map[int64]cachedAccountClient
	retired []retiredClient
	// resolve returns the account's current state so the cache can decide whether
	// the client it holds is still the right one. It is injected by the handler,
	// which owns the store.
	resolve func(id int64) *store.Account
	// cfg is the config the client fingerprint is computed with.
	cfg *config.Config
}

// SetConfig wires the config used when re-checking whether a cached client is
// still built from the account's current state.
func (c *accountClientCache) SetConfig(cfg *config.Config) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.cfg = cfg
	c.mu.Unlock()
}

// SetAccountResolver wires the reader the cache uses to re-check an account when
// it is told the account changed.
func (c *accountClientCache) SetAccountResolver(resolve func(id int64) *store.Account) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.resolve = resolve
	c.mu.Unlock()
}

// AccountChanges implements the account-change subscriber contract.
func (h *Handler) AccountChanges(ids []int64) {
	if h == nil || h.clientCache == nil {
		return
	}
	h.clientCache.evictAccounts(ids)
}

// AccountChangeBatch invalidates the client cache for every account the
// account-events bus reports as changed.
func (h *Handler) AccountChangeBatch(changes []accountevents.Change) {
	ids := make([]int64, 0, len(changes))
	for _, change := range changes {
		if change.AccountID == 0 {
			continue
		}
		ids = append(ids, change.AccountID)
	}
	h.AccountChanges(ids)
}

type clientCloser interface {
	Close()
}

func newAccountClientCache() *accountClientCache {
	return &accountClientCache{entries: make(map[int64]cachedAccountClient)}
}

// acquireAccountClient resolves the client for one request and marks it in use.
// The returned release function must always be called: it is what closes a client
// that was evicted while the request was running.
func (h *Handler) acquireAccountClient(acc *store.Account) (UpstreamClient, func()) {
	client := h.getOrCreateAccountClient(acc)
	if client == nil {
		return nil, func() {}
	}
	if h == nil || h.clientCache == nil || acc == nil || acc.ID == 0 {
		return client, func() {}
	}

	h.clientCache.mu.Lock()
	if entry, ok := h.clientCache.entries[acc.ID]; ok && entry.client == client {
		entry.inUse++
		h.clientCache.entries[acc.ID] = entry
	}
	h.clientCache.mu.Unlock()

	var once sync.Once
	return client, func() {
		once.Do(func() { h.clientCache.release(acc.ID, client) })
	}
}

// release drops one use of a client and closes it when it is no longer current
// and nobody else holds it.
func (c *accountClientCache) release(accountID int64, client UpstreamClient) {
	if c == nil || client == nil {
		return
	}
	closer, closable := client.(clientCloser)

	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.entries[accountID]; ok && entry.client == client {
		if entry.inUse > 0 {
			entry.inUse--
		}
		c.entries[accountID] = entry
		return
	}

	// The entry was evicted while the request was running: close it as soon as the
	// last holder is done.
	for i := range c.retired {
		if c.retired[i].client != client {
			continue
		}
		if c.retired[i].inUse > 0 {
			c.retired[i].inUse--
		}
		if c.retired[i].inUse == 0 {
			c.retired = append(c.retired[:i], c.retired[i+1:]...)
			if closable {
				closer.Close()
			}
		}
		return
	}
}

// evictAccounts is the subscriber entry point. It resolves each account's current
// state and keeps a client whose credential has not actually moved.
//
// A blind "something changed" eviction would rebuild a client for a rename or a
// status update and drop the keep-alive pool for nothing; a blind "always keep"
// would keep serving a replaced credential. The fingerprint is the deciding
// signal, and it is re-derived from the account rather than trusted from the
// event, so a stale event cannot resurrect an old client.
func (c *accountClientCache) evictAccounts(ids []int64) {
	if c == nil || len(ids) == 0 {
		return
	}
	c.mu.RLock()
	resolve := c.resolve
	c.mu.RUnlock()

	for _, id := range ids {
		if id == 0 {
			continue
		}
		var account *store.Account
		if resolve != nil {
			account = resolve(id)
		}
		if account == nil {
			// The account is gone (or unreadable): never keep serving its client.
			c.evictByID(id)
			continue
		}
		c.evictIfChanged(account)
	}
}

// evictIfChanged drops the entry when the account no longer builds the same
// client. It reports whether the entry was dropped.
func (c *accountClientCache) evictIfChanged(account *store.Account) bool {
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()
	fingerprint := accountClientFingerprint(account, cfg)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[account.ID]
	if !ok || entry.fingerprint == fingerprint {
		return false
	}
	c.dropLocked(account.ID, entry)
	return true
}

// evictByID drops an entry without re-reading the account.
func (c *accountClientCache) evictByID(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[id]
	if !ok {
		return
	}
	c.dropLocked(id, entry)
}

// dropLocked removes an entry, retiring a client that is still in use instead of
// closing it under a running request.
func (c *accountClientCache) dropLocked(id int64, entry cachedAccountClient) {
	delete(c.entries, id)
	if entry.client == nil {
		return
	}
	if entry.inUse > 0 {
		c.retired = append(c.retired, retiredClient{client: entry.client, inUse: entry.inUse})
		return
	}
	if closer, ok := entry.client.(clientCloser); ok {
		closer.Close()
	}
}

func (h *Handler) getOrCreateAccountClient(acc *store.Account) UpstreamClient {
	if acc == nil {
		return nil
	}
	if h == nil || h.clientCache == nil || acc.ID == 0 {
		return h.buildAccountClient(acc)
	}

	cfg := h.configSnapshot()
	fingerprint := accountClientFingerprint(acc, cfg)

	h.clientCache.mu.RLock()
	entry, ok := h.clientCache.entries[acc.ID]
	h.clientCache.mu.RUnlock()
	if ok && entry.fingerprint == fingerprint && entry.client != nil {
		return entry.client
	}

	client := h.buildAccountClient(acc)
	if client == nil {
		return nil
	}

	h.clientCache.mu.Lock()
	defer h.clientCache.mu.Unlock()

	if entry, ok := h.clientCache.entries[acc.ID]; ok && entry.fingerprint == fingerprint && entry.client != nil {
		if c, ok := client.(clientCloser); ok {
			c.Close()
		}
		return entry.client
	}

	if entry, ok := h.clientCache.entries[acc.ID]; ok && entry.client != nil {
		// The entry is being replaced: a running request keeps the old client.
		if entry.inUse > 0 {
			h.clientCache.retired = append(h.clientCache.retired, retiredClient{client: entry.client, inUse: entry.inUse})
		} else if closer, closable := entry.client.(clientCloser); closable {
			closer.Close()
		}
	}

	h.clientCache.entries[acc.ID] = cachedAccountClient{
		fingerprint: fingerprint,
		client:      client,
	}

	return client
}

func (h *Handler) buildAccountClient(acc *store.Account) UpstreamClient {
	if acc == nil {
		return nil
	}
	var cfg *config.Config
	if h != nil {
		cfg = h.configSnapshot()
	}
	if h != nil && h.clientFactory != nil {
		return h.clientFactory(acc, cfg)
	}
	// The constructor table lives in internal/provider, which is the same seam
	// cmd/server uses. Repeating it here meant a fourth provider would have to
	// be added in two places that could disagree.
	factory, ok := provider.Get(acc.AccountType)
	if !ok {
		return nil
	}
	client, ok := factory(acc, cfg).(UpstreamClient)
	if !ok {
		return nil
	}
	// A rotating credential has to be written back: WorkBuddy rotates its token
	// on every renewal, Qoder on its refresh, and Cline the same. Without the
	// store the client would keep using a credential the pool already replaced.
	if setter, wants := client.(interface{ SetAccountStore(*store.Store) }); wants &&
		h != nil && h.loadBalancer != nil && h.loadBalancer.Store != nil {
		setter.SetAccountStore(h.loadBalancer.Store)
	}
	return client
}

func (h *Handler) Close() {
	if h == nil {
		return
	}

	h.statsOnce.Do(h.initAccountStatsWriter)
	h.statsCloseOnce.Do(func() {
		h.statsMu.Lock()
		h.statsClosed = true
		h.statsMu.Unlock()
		close(h.statsStop)
		<-h.statsDone
		h.flushPendingAccountStats(5 * time.Second)
	})

	closers := make([]clientCloser, 0, 1)
	if c, ok := h.client.(clientCloser); ok {
		closers = append(closers, c)
	}

	if h.clientCache != nil {
		h.clientCache.mu.Lock()
		for _, entry := range h.clientCache.entries {
			if c, ok := entry.client.(clientCloser); ok {
				closers = append(closers, c)
			}
		}
		for _, client := range h.clientCache.retired {
			if c, ok := client.client.(clientCloser); ok {
				closers = append(closers, c)
			}
		}
		h.clientCache.entries = make(map[int64]cachedAccountClient)
		h.clientCache.retired = nil
		h.clientCache.mu.Unlock()
	}

	for _, c := range closers {
		c.Close()
	}
}

func accountClientFingerprint(acc *store.Account, cfg *config.Config) string {
	if acc == nil {
		return ""
	}

	hasher := sha256.New()
	writeString := func(value string) {
		_, _ = io.WriteString(hasher, value)
		_, _ = hasher.Write([]byte{0})
	}
	writeBool := func(value bool) {
		if value {
			_, _ = hasher.Write([]byte{1})
		} else {
			_, _ = hasher.Write([]byte{0})
		}
		_, _ = hasher.Write([]byte{0})
	}
	writeInt64 := func(value int64) {
		_, _ = io.WriteString(hasher, strconv.FormatInt(value, 10))
		_, _ = hasher.Write([]byte{0})
	}
	writeInt := func(value int) { writeInt64(int64(value)) }
	writeStrings := func(values []string) {
		writeInt(len(values))
		for _, value := range values {
			writeString(value)
		}
	}

	writeInt64(acc.ID)
	writeString(acc.Name)
	writeString(acc.AccountType)
	writeString(acc.ClientCookie)
	writeString(acc.RefreshToken)
	writeString(acc.UserID)
	writeString(acc.AgentMode)
	writeString(acc.Email)
	writeString(acc.Token)
	writeString(acc.WorkBuddyAccessToken)
	writeString(acc.WorkBuddyRefreshToken)
	writeString(acc.WorkBuddyUID)
	writeInt64(acc.WorkBuddyExpiresAt.UnixNano())
	writeStrings(acc.WorkBuddyModelIDs)
	writeString(acc.QoderAccessToken)
	writeString(acc.QoderRefreshToken)
	writeInt64(acc.QoderExpiresAt.UnixNano())
	writeString(acc.QoderMachineID)
	writeString(acc.QoderUserID)
	writeString(acc.QoderUserName)
	writeString(acc.QoderOrganizationID)
	writeStrings(acc.QoderOrganizationTags)
	writeBool(acc.QoderDataPolicy)
	// RuntimeInfo/RuntimeKey are random derived output written by the Qoder
	// client itself. Including them invalidates that client on its first write,
	// causing the next request to derive and persist another pair indefinitely.
	// Token, UID and device inputs above still force a rebuild when they change.
	writeStrings(acc.QoderModelIDs)
	// The catalog observation timestamp does not change model resolution.
	writeString(acc.ClineAccessToken)
	writeString(acc.ClineRefreshToken)
	writeInt64(acc.ClineExpiresAt.UnixNano())
	writeString(acc.ClineEmail)
	writeStrings(acc.ClineModelIDs)
	// Do not include stats-only timestamps like UpdatedAt here.
	// Request/usage accounting bumps UpdatedAt on every call, and using it in the
	// fingerprint would force unnecessary client rebuilds and drop keep-alive pools.

	if cfg != nil {
		writeString(cfg.WorkBuddyBaseURL)
		writeString(cfg.QoderOAuthBaseURL)
		writeString(cfg.QoderOpenAPIBaseURL)
		writeString(cfg.QoderInferenceURL)
		writeString(cfg.QoderClientID)
		writeString(cfg.QoderClientVersion)
		writeString(cfg.ClineAPIBaseURL)
		writeString(cfg.ClineWorkOSClientID)
		writeString(cfg.ClineWorkOSAuthorizeURL)
		writeString(cfg.ClineWorkOSTokenURL)
		writeString(cfg.ProxyURL)
		writeString(cfg.ProxyHTTP)
		writeString(cfg.ProxyHTTPS)
		writeString(cfg.ProxyUser)
		writeString(cfg.ProxyPass)
		// Handler-level runtime controls are read on each request, not captured
		// by these provider clients. RequestTimeout is captured at construction.
		writeInt(cfg.RequestTimeout)
		writeInt(cfg.WorkBuddyDefaultMaxTokens)
		writeInt(cfg.SharedStreamIdleTimeoutSeconds)
		writeInt(cfg.UpstreamMaxConnsPerHost)
		writeInt(cfg.UpstreamMaxIdleConnsPerHost)
		writeString(strconv.FormatBool(cfg.QoderHTTP2Enabled))
		writeString(strconv.FormatBool(cfg.ClineHTTP2Enabled))
		writeString(strconv.FormatBool(cfg.WorkBuddyHTTP2Enabled))
		for _, value := range cfg.ProxyBypass {
			writeString(value)
		}
	}

	return hex.EncodeToString(hasher.Sum(nil))
}
