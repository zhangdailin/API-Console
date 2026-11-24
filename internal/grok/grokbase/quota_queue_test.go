package grokbase

import (
	"testing"

	"github.com/alicebob/miniredis/v2"

	"orchids-api/internal/store"
)

func TestQuotaJobsKeepLocalAndObservedUsageSeparate(t *testing.T) {
	s, err := store.New(store.Options{RedisAddr: miniredis.RunT(t).Addr()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	for _, plane := range []string{store.GrokProviderBuild, store.GrokProviderWeb} {
		t.Run(plane, func(t *testing.T) {
			window := func(a *store.Account) *store.GrokQuotaWindow {
				if plane == store.GrokProviderBuild {
					return &a.GrokRateLimits.Requests
				}
				return &a.GrokWebQuota.Auto
			}
			account := &store.Account{AccountType: "grok", GrokProvider: plane, Enabled: true}
			*window(account) = store.GrokQuotaWindow{Remaining: 20, HasRemaining: true}
			if err := s.CreateAccount(t.Context(), account); err != nil {
				t.Fatal(err)
			}
			load := func() *store.Account {
				t.Helper()
				row, err := s.GetAccount(t.Context(), account.ID)
				if err != nil {
					t.Fatal(err)
				}
				return row
			}
			job := QuotaJob{Account: *account, Requests: 3}
			job.Persist(s, plane, false, nil)
			row := load()
			if row.RequestCount != 3 || window(row).Remaining != 17 {
				t.Fatalf("local count %d, remaining %v", row.RequestCount, window(row).Remaining)
			}

			// The queued job holds a stale account; a fresh observed allowance
			// must be combined with the latest row and its usage counter.
			row.Enabled = false
			if err := s.UpdateAccount(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			job.Requests = 2
			observedQuota := func(row *store.Account) *float64 {
				if plane == store.GrokProviderBuild {
					return &row.GrokRateLimits.Requests.Remaining
				}
				return &row.UsageCurrent
			}
			job.Persist(s, plane, true, func(row *store.Account) bool {
				*observedQuota(row) = 12
				return true
			})
			row = load()
			if remaining := *observedQuota(row); row.RequestCount != 5 || remaining != 12 || row.Enabled {
				t.Fatalf("observed count %d, remaining %v, enabled %v", row.RequestCount, remaining, row.Enabled)
			}
			if plane == store.GrokProviderWeb && window(row).Remaining != 17 {
				t.Fatalf("observed quota also consumed local Web allowance: %v", window(row).Remaining)
			}
		})
	}
}
