package refresh

import (
	"context"
	"testing"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/provider"
	_ "orchids-api/internal/provider/all"
	"orchids-api/internal/store"
)

// TestMaintainAccountsToleratesWhatTheStoreCanReturn covers the rows the loop
// must step over rather than fail on: a nil entry, an account type no channel
// owns, and an account of every registered channel when there is no store to
// write to. None of them may reach an upstream or panic.
func TestMaintainAccountsToleratesWhatTheStoreCanReturn(t *testing.T) {
	accounts := []*store.Account{nil, {ID: 1, AccountType: "retired-channel", Enabled: true}}
	for index, definition := range channel.All() {
		accounts = append(accounts, &store.Account{ID: int64(index + 2), AccountType: string(definition.ID), Enabled: true})
	}
	maintainAccounts(context.Background(), provider.Env{Config: &config.Config{}}, accounts)
}
