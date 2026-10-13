package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/channel"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// Recognising an account this gateway already has, so one upstream login is one
// row. Deduplication runs on a stable provider identity as well as the
// credential, because a fresh login rotates the durable token.

func normalizedAccountCredentialKey(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	if admin := adminFor(acc.AccountType); admin != nil && admin.CredentialKey != nil {
		return admin.CredentialKey(acc)
	}

	accountType := strings.ToLower(strings.TrimSpace(acc.AccountType))
	token := strings.TrimSpace(util.FirstNonEmpty(acc.RefreshToken, acc.ClientCookie, acc.Token))
	if token == "" || accountType == "" {
		return ""
	}
	return accountType + ":" + token
}

func isSupportedAccountType(accountType string) bool { return channel.IsSupported(accountType) }

// validateAccountType rejects an account whose type is missing or unknown.
//
// The create and update surfaces both take an account type from the request
// body, and both have to answer the same two questions before touching the
// store: is a type present, and is it one this gateway serves. Reporting the
// error and writing the response belongs here so the two surfaces cannot drift
// into giving different answers about the same input.
func validateAccountType(w http.ResponseWriter, accountType string) bool {
	if strings.TrimSpace(accountType) == "" {
		http.Error(w, "account_type is required", http.StatusBadRequest)
		return false
	}
	if !isSupportedAccountType(accountType) {
		http.Error(w, "unsupported account type", http.StatusBadRequest)
		return false
	}
	return true
}

func (a *API) findDuplicateAccountByCredential(ctx context.Context, acc *store.Account, excludeID int64) (*store.Account, error) {
	if a == nil || a.store == nil || acc == nil {
		return nil, nil
	}

	key := normalizedAccountCredentialKey(acc)
	identityKey := stableProviderIdentityKey(acc)
	if key == "" && identityKey == "" {
		return nil, nil
	}

	accounts, err := a.store.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	for _, existing := range accounts {
		if existing == nil || existing.ID == excludeID {
			continue
		}
		if identityKey != "" && stableProviderIdentityKey(existing) == identityKey {
			return existing, nil
		}
		if key != "" && normalizedAccountCredentialKey(existing) == key {
			return existing, nil
		}
	}
	return nil, nil
}

// saveNewAccountUnlessDuplicate stores a freshly authenticated account, or
// returns the row that already carries its credential.
//
// A completed device login and a completed browser login reach the same
// decision — an upstream may hand out a second grant for an account this
// gateway already has, and inserting it would give the scheduler two rows for
// one allowance. The duplicate check and the insert share a single deadline
// because they are one step: leaving it to the caller's context would let a
// login hold a store round-trip open for the whole poll lifetime.
func (a *API) saveNewAccountUnlessDuplicate(ctx context.Context, acc *store.Account) (*store.Account, error) {
	storeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	existing, err := a.findDuplicateAccountByCredential(storeCtx, acc, 0)
	if err == nil && existing == nil {
		err = a.store.CreateAccount(storeCtx, acc)
	}
	return existing, err
}

// stableProviderIdentityKey survives OAuth token rotation. A channel issues a
// new durable token during a fresh login, so token-only deduplication would
// create a second row for the same upstream user and leave the old row holding
// a consumed refresh token.
func stableProviderIdentityKey(acc *store.Account) string {
	if acc == nil {
		return ""
	}
	if admin := adminFor(acc.AccountType); admin != nil && admin.IdentityKey != nil {
		return admin.IdentityKey(acc)
	}
	return ""
}

func duplicateAccountError(existing *store.Account) error {
	if existing == nil {
		return fmt.Errorf("duplicate account token")
	}
	accountType := strings.TrimSpace(existing.AccountType)
	accountType = util.FirstNonEmptyUntrimmed(accountType, "account")
	return fmt.Errorf("duplicate %s token already exists on account #%d", accountType, existing.ID)
}
