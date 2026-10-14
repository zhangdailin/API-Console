package api

import (
	"fmt"
	"sync"
	"testing"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

func TestConcurrentReplicaLoginsCreateOnlyOneAccount(t *testing.T) {
	for _, stable := range []bool{false, true} {
		t.Run(fmt.Sprint(stable), func(t *testing.T) {
			s, _ := newTestStore(t, "atomic-account:")
			other, err := store.New(store.Options{RedisAddr: s.RedisClient().Options().Addr, RedisPrefix: s.RedisPrefix(), CredentialEncryptionKey: []byte("01234567890123456789012345678901")})
			testutil.NoError(t, err)
			t.Cleanup(func() { other.Close() })
			replicas := []*API{New(s, "", "", &config.Config{}), New(other, "", "", &config.Config{})}
			start := make(chan struct{})
			results := make(chan error, 8)
			created := make(chan bool, 8)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Go(func() {
					<-start
					acc := &store.Account{AccountType: "workbuddy", WorkBuddyRefreshToken: "same-refresh", Enabled: true}
					if stable {
						acc.WorkBuddyUID = "same-user"
						acc.WorkBuddyRefreshToken = fmt.Sprintf("rotated-%d", i)
					}
					duplicate, err := replicas[i%2].saveNewAccountUnlessDuplicate(t.Context(), acc)
					results <- err
					created <- duplicate == nil
				})
			}
			close(start)
			wg.Wait()
			close(results)
			close(created)
			for err := range results {
				testutil.NoError(t, err)
			}
			count := 0
			for wasCreated := range created {
				if wasCreated {
					count++
				}
			}
			testutil.Equal(t, count, 1)
			rows, err := s.ListAccounts(t.Context())
			testutil.NoError(t, err)
			testutil.Equal(t, len(rows), 1)
		})
	}
}
