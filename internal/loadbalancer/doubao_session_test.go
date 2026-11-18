package loadbalancer

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/store"
)

func TestDoubaoLateAuthVerdictDoesNotPoisonNewSessionOrCache(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-session", true: "new-login"}[rotate], func(t *testing.T) {
			mini := miniredis.RunT(t)
			s, err := store.New(store.Options{RedisAddr: mini.Addr(), RedisPrefix: "doubao-routing:", CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			acc := &store.Account{AccountType: "doubao", Enabled: true, Name: "admin", Weight: 2, ClientCookie: `{"sessionid":"synthetic-old"}`}
			if err := s.CreateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			stale := acc.Clone()
			acc.Enabled, acc.Name, acc.Weight = false, "new admin name", 9
			if rotate {
				acc.ClientCookie = `{"sessionid":"synthetic-new"}`
			}
			if err := s.UpdateAccount(t.Context(), acc); err != nil {
				t.Fatal(err)
			}
			lb := &LoadBalancer{Store: s, cachedAccounts: []*store.Account{acc.Clone()}}
			accountpolicy.Verdict{Status: "401", NeedsLogin: true, At: time.Now()}.Apply(stale)
			lb.PersistAppliedAccountStatus(t.Context(), stale, "synthetic failure")
			got, err := s.GetAccount(t.Context(), acc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Enabled || got.Name != "new admin name" || got.Weight != 9 {
				t.Fatal("request failure overwrote administrator settings")
			}
			for _, observed := range []*store.Account{got, lb.cachedAccounts[0]} {
				if rotate && (!store.AccountAuthActive(observed) || observed.StatusCode != "") {
					t.Fatal("old failure poisoned new login")
				}
				if !rotate && (store.AccountAuthActive(observed) || observed.StatusCode != "401") {
					t.Fatal("current rejection was lost")
				}
			}
		})
	}
}
