package doubao

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/refreshqueue"
	"orchids-api/internal/store"
)

var (
	errSessionRejected    = errors.New("status=401: doubao cookie was rejected")
	errSessionChanged     = errors.New("doubao protocol error: session changed during request; no automatic retry")
	errSessionPersistence = errors.New("doubao protocol error: cookie persistence failed; no automatic retry")
)

func newClientWithStore(acc *store.Account, cfg *config.Config, s *store.Store) *Client {
	c := NewFromAccount(acc, cfg)
	c.store = s
	return c
}

// Public client entry points use private request snapshots; cached clients never
// mutate their shared account, HTTP client, or credentials during inference.
func (c *Client) scopedClient() *Client {
	if c == nil {
		return &Client{}
	}
	return &Client{acc: c.acc.Clone(), cfg: c.cfg, http: c.http, store: c.store, sessionVersion: c.sessionVersion}
}

func (c *Client) trustedSessionURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil {
		return false
	}
	for _, base := range []string{c.baseURL(), c.accountsURL()} {
		b, err := url.Parse(base)
		if err == nil && u.Scheme == b.Scheme && u.Host == b.Host {
			return true
		}
	}
	return false
}

// Serialize only through response headers and cookie commit, not the streamed
// body. Independent accounts and long-running answer streams do not share a lock.
func (c *Client) prepareSession(ctx context.Context, target *string, jar cookieJar) (func(), error) {
	if !c.trustedSessionURL(*target) {
		return nil, errSessionChanged
	}
	if c.store == nil || c.acc == nil || c.acc.ID == 0 {
		return func() {}, nil
	}
	release, err := refreshqueue.AcquireCredential(ctx, "doubao", c.store, c.acc.ID)
	if err != nil {
		return nil, err
	}
	latest, err := c.store.GetAccount(ctx, c.acc.ID)
	if err != nil || latest == nil {
		release()
		return nil, errSessionPersistence
	}
	if latest.AccountType != "doubao" || latest.DoubaoDeviceID != c.acc.DoubaoDeviceID || latest.DoubaoWebID != c.acc.DoubaoWebID || latest.DoubaoTeaUUID != c.acc.DoubaoTeaUUID {
		release()
		return nil, errSessionChanged
	}
	u, _ := url.Parse(*target)
	health := u.Path == "/passport/account/info/v2/"
	if !health && (!latest.Enabled || !store.AccountAuthActive(latest)) {
		release()
		return nil, errSessionChanged
	}
	for key := range jar {
		delete(jar, key)
	}
	for key, value := range decodeCookies(latest.ClientCookie) {
		jar[key] = value
	}
	seedPreferences(jar)
	c.sessionVersion = store.DoubaoSessionVersion(latest)
	query := u.Query()
	if query.Has("device_id") {
		if fp := jar["s_v_web_id"]; fp != "" {
			query.Set("fp", fp)
		} else {
			query.Del("fp")
		}
		u.RawQuery = query.Encode()
		*target = u.String()
	}
	return release, nil
}

func (c *Client) receiveSession(ctx context.Context, response *http.Response, jar cookieJar) error {
	before := jar.encode()
	jar.absorb(response)
	if before == jar.encode() {
		return nil
	}
	if c.store == nil || c.acc == nil || c.acc.ID == 0 {
		return nil
	}
	patch := store.DoubaoSessionPatch{Expected: c.sessionVersion, CookieChanged: true, Cookie: jar.encode()}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.store.UpdateDoubaoSession(writeCtx, c.acc.ID, patch); err != nil {
		return errSessionPersistence
	}
	snapshot := c.acc.Clone()
	snapshot.ClientCookie = patch.Cookie
	c.sessionVersion = store.DoubaoSessionVersion(snapshot)
	return nil
}

func (info AccountInfo) reject(target *store.Account, now time.Time, err error) {
	if target != nil && target.ID != 0 && errors.Is(err, errSessionRejected) && info.version != ([32]byte{}) {
		target.DoubaoSessionUpdate = &store.DoubaoSessionObservation{Expected: info.version, Rejected: true, ObservedAt: now}
	}
}
