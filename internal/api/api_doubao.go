package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"orchids-api/internal/provider"
	"orchids-api/internal/util"
)

func (a *API) HandleDoubaoCookieImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMethodNotAllowed(w)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		Cookie  string `json:"cookie"`
		Enabled *bool  `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") ||
		decoder.Decode(&input) != nil || !errors.Is(decoder.Decode(new(json.RawMessage)), io.EOF) ||
		strings.TrimSpace(input.Cookie) == "" {
		writeLoginError(w, http.StatusBadRequest, "invalid_cookie_import", "Provide a valid Doubao browser Cookie")
		return
	}
	spec, ok := provider.Lookup("doubao")
	if !ok || spec.Admin == nil || spec.Admin.ImportBrowserCookie == nil {
		writeLoginError(w, http.StatusServiceUnavailable, "doubao_unavailable", "Doubao account import is unavailable")
		return
	}
	env := provider.Env{Config: a.ConfigSnapshot(), Store: a.store}
	account, err := spec.Admin.ImportBrowserCookie(r.Context(), env, input.Cookie)
	if err != nil {
		writeLoginError(w, http.StatusUnprocessableEntity, "doubao_cookie_unverified", "Doubao did not verify the supplied Cookie; sign in to Doubao again and retry")
		return
	}
	if input.Enabled != nil {
		account.Enabled = *input.Enabled
	}
	existing, err := a.saveNewAccountUnlessDuplicate(r.Context(), account)
	if err != nil {
		writeLoginError(w, http.StatusServiceUnavailable, "account_save_failed", "Could not save the verified Doubao account")
		return
	}
	if existing != nil {
		writeLoginError(w, http.StatusConflict, "duplicate_account", "This Doubao account already exists; use QR login to refresh it")
		return
	}
	a.syncAccountAfterCreate(*account)
	util.WriteJSONStatus(w, http.StatusCreated, normalizeAccountOutput(account))
}
