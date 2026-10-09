package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func (a *API) HandleExport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}

	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	exportData := ExportData{
		Version:  1,
		ExportAt: time.Now(),
		Accounts: make([]store.Account, 0, len(accounts)),
	}
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		normalized := *acc
		// Export the stored state, not the management projection: projection
		// redaction would remove credentials required to restore the account.
		normalizePortableAccount(&normalized)
		redactForeignCredentials(&normalized)
		normalized.ID = 0
		normalized.RequestCount = 0
		exportData.Accounts = append(exportData.Accounts, normalized)
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", "attachment; filename=accounts_export.json")
	util.WriteJSON(w, exportData)
}

// normalizePortableAccount validates provider-owned restore credentials without
// upstream calls. A durable refresh credential is required for a restorable row.
func normalizePortableAccount(acc *store.Account) string {
	acc.AccountType = strings.ToLower(strings.TrimSpace(acc.AccountType))
	defer func() { acc.Token = ""; acc.RefreshToken = ""; acc.ClientCookie = "" }()
	switch acc.AccountType {
	case "workbuddy":
		if !NormalizeWorkBuddyCredentials(acc) || strings.TrimSpace(acc.WorkBuddyRefreshToken) == "" {
			return "missing_refresh_token"
		}
	case "qoder":
		if !NormalizeQoderCredentials(acc) || strings.TrimSpace(acc.QoderRefreshToken) == "" {
			return "missing_refresh_token"
		}
		if strings.TrimSpace(acc.QoderMachineID) == "" {
			return "missing_device_identity"
		}
	case "cline":
		if !NormalizeClineCredentials(acc) || strings.TrimSpace(acc.ClineRefreshToken) == "" {
			return "missing_refresh_token"
		}
	case "grok":
		normalizeGrokTokenInput(acc)
		if strings.TrimSpace(acc.OAuthRefreshToken) == "" {
			return "missing_refresh_token"
		}
	default:
		return "unsupported_channel"
	}
	return ""
}

// redactForeignCredentials clears the credential fields of channels other than
// the account's own.
//
// The account read path gets this for free: accountOutput.MarshalJSON deletes the
// credential keys from the response object, whichever channel they belong to. The
// export marshals the stored record instead of going through that marshaler, so
// it needs the same guarantee expressed as data. It matters because a legacy row
// can hold a value in a slot its own channel never writes — the reason
// RedactQoderOutput clears the generic slots at all — and without this the export
// would publish it.
//
// Generic credential slots are cleared before this function is called.
func redactForeignCredentials(acc *store.Account) {
	if acc == nil {
		return
	}
	channel := strings.ToLower(strings.TrimSpace(acc.AccountType))
	// Grok's OAuth pair is restored above for an OAuth account specifically, so
	// an SSO row is treated like any other row that has no claim to it.
	if !(channel == "grok" && grokAccountIsOAuth(acc)) {
		acc.OAuthAccessToken = ""
		acc.OAuthRefreshToken = ""
		acc.OAuthExpiresAt = time.Time{}
	}
	if channel != "workbuddy" {
		acc.WorkBuddyAccessToken = ""
		acc.WorkBuddyRefreshToken = ""
		acc.WorkBuddyExpiresAt = time.Time{}
	}
	if channel != "qoder" {
		acc.QoderAccessToken = ""
		acc.QoderRefreshToken = ""
		acc.QoderExpiresAt = time.Time{}
		acc.QoderRuntimeInfo = ""
		acc.QoderRuntimeKey = ""
	}
	if channel != "cline" {
		acc.ClineAccessToken = ""
		acc.ClineRefreshToken = ""
		acc.ClineExpiresAt = time.Time{}
	}
}

func (a *API) HandleImport(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	decoder := json.NewDecoder(r.Body)
	var exportData ExportData
	if err := decoder.Decode(&exportData); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Import file exceeds 8 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Invalid import JSON", http.StatusBadRequest)
		}
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		http.Error(w, "Import must contain one JSON document", http.StatusBadRequest)
		return
	}
	if exportData.Version != 1 || exportData.Accounts == nil {
		http.Error(w, "Expected a version 1 account export", http.StatusBadRequest)
		return
	}
	// Serialize imports in this process. Existing rows are never overwritten.
	a.importMu.Lock()
	defer a.importMu.Unlock()
	existing, err := a.store.ListAccounts(r.Context())
	if err != nil {
		http.Error(w, "Cannot read existing accounts", http.StatusServiceUnavailable)
		return
	}
	credentials, identities := map[string]bool{}, map[string]bool{}
	remember := func(acc *store.Account) {
		if key := normalizedAccountCredentialKey(acc); key != "" {
			credentials[key] = true
		}
		if key := stableProviderIdentityKey(acc); key != "" {
			identities[key] = true
		}
	}
	for _, acc := range existing {
		remember(acc)
	}
	result := ImportResult{Total: len(exportData.Accounts)}
	skip := func(index int, reason string) {
		result.Skipped++
		result.Issues = append(result.Issues, ImportIssue{Index: index + 1, Reason: reason})
	}
	for index, acc := range exportData.Accounts {
		reason := normalizePortableAccount(&acc)
		if reason != "" {
			result.Invalid++
			skip(index, reason)
			continue
		}
		redactForeignCredentials(&acc)
		if credentials[normalizedAccountCredentialKey(&acc)] || identities[stableProviderIdentityKey(&acc)] {
			result.Duplicates++
			skip(index, "duplicate_account")
			continue
		}
		acc.ID = 0
		acc.RequestCount = 0
		acc.TokensToday = 0
		acc.TokensDate = ""
		acc.UpdatedAt = time.Time{}
		if acc.Weight <= 0 {
			acc.Weight = 1
		}
		if err := a.store.CreateAccount(r.Context(), &acc); err != nil {
			slog.Warn("Failed to import account", "index", index+1, "channel", acc.AccountType)
			result.Failed++
			skip(index, "storage_error")
		} else {
			result.Imported++
			remember(&acc)
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	util.WriteJSON(w, result)
}
