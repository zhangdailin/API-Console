package handler

import (
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func doubaoCacheAccount() *store.Account {
	return &store.Account{
		ID: 43, AccountType: "doubao", ClientCookie: `{"sessionid":"test-session","s_v_web_id":"test-fp"}`,
		DoubaoDeviceID: "1000000000000000001", DoubaoWebID: "1000000000000000002", DoubaoTeaUUID: "1000000000000000002",
	}
}

func TestDoubaoClientFingerprintIncludesAllIdentityIDs(t *testing.T) {
	base := doubaoCacheAccount()
	cfg := &config.Config{}
	before := accountClientFingerprint(base, cfg)
	for name, rotate := range map[string]func(*store.Account){
		"device": func(acc *store.Account) { acc.DoubaoDeviceID = "2000000000000000001" },
		"web":    func(acc *store.Account) { acc.DoubaoWebID = "2000000000000000002" },
		"tea":    func(acc *store.Account) { acc.DoubaoTeaUUID = "2000000000000000003" },
	} {
		t.Run(name, func(t *testing.T) {
			next := base.Clone()
			rotate(next)
			testutil.NotEqual(t, accountClientFingerprint(next, cfg), before)
		})
	}
	next := base.Clone()
	next.UpdatedAt = time.Now()
	testutil.Equal(t, accountClientFingerprint(next, cfg), before)
}

func TestDoubaoIdentityRotationRebuildsCachedClient(t *testing.T) {
	for name, rotate := range map[string]func(*store.Account){
		"device": func(acc *store.Account) { acc.DoubaoDeviceID = "2000000000000000001" },
		"web":    func(acc *store.Account) { acc.DoubaoWebID = "2000000000000000002" },
		"tea":    func(acc *store.Account) { acc.DoubaoTeaUUID = "2000000000000000003" },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCacheTestHandler(t)
			original := doubaoCacheAccount()
			first, releaseFirst := fixture.handler.acquireAccountClient(original)
			releaseFirst()
			unchanged, releaseUnchanged := fixture.handler.acquireAccountClient(original.Clone())
			releaseUnchanged()
			testutil.Equal(t, unchanged, first)
			testutil.Equal(t, len(fixture.builtClients()), 1)

			rotated := original.Clone()
			rotate(rotated)
			fixture.setCurrent(rotated)
			fixture.handler.AccountChanges([]int64{rotated.ID})
			second, releaseSecond := fixture.handler.acquireAccountClient(rotated)
			defer releaseSecond()
			testutil.NotEqual(t, second, first)
			testutil.Equal(t, len(fixture.builtClients()), 2)
			testutil.True(t, fixture.builtClients()[0].isClosed(), "rotated idle client was not closed")
		})
	}
}
