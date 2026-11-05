package grok

import (
	"context"
	"strings"

	"orchids-api/internal/grok/egressnodes"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
)

// egressAccountLister enumerates Grok accounts with the affinity each egress
// manager keys them by: Web accounts by their SSO cookie, Build accounts by
// their CLI identity. Disabled accounts are not routed and are not bound.
func egressAccountLister(st *store.Store) egressnodes.AccountLister {
	return func(ctx context.Context) ([]egressnodes.AccountRef, error) {
		accounts, err := st.ListAccounts(ctx)
		if err != nil {
			return nil, err
		}
		var out []egressnodes.AccountRef
		for _, acc := range accounts {
			if acc == nil || !acc.Enabled || !strings.EqualFold(strings.TrimSpace(acc.AccountType), "grok") {
				continue
			}
			if web.IsWebAccount(acc) {
				if affinity := web.EgressAffinity(acc); affinity != "" {
					out = append(out, egressnodes.AccountRef{Scope: store.GrokEgressScopeWeb, Affinity: affinity, AccountID: acc.ID})
				}
				continue
			}
			if affinity := cliEgressAffinity(acc); affinity != "" {
				out = append(out, egressnodes.AccountRef{Scope: store.GrokEgressScopeBuild, Affinity: affinity, AccountID: acc.ID})
			}
		}
		return out, nil
	}
}
