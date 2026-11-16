package api

import (
	"net/http"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// handleDeviceIdentityRotation is admin-session protected by the account route.
// It updates only the local identity tuple, never upstream credentials or parks.
func (a *API) handleDeviceIdentityRotation(w http.ResponseWriter, r *http.Request, account *store.Account) {
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	admin := adminFor(account.AccountType)
	if admin == nil || admin.RotateDeviceIdentity == nil {
		util.WriteJSONStatus(w, http.StatusNotFound, map[string]string{"error": "device identity rotation is not supported for this channel"})
		return
	}
	if !sameOriginAdminRequest(w, r, "device identity rotation") {
		return
	}
	patch, err := admin.RotateDeviceIdentity(account)
	if err != nil {
		util.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{"error": "could not generate a new device identity"})
		return
	}
	if err := a.store.RotateDoubaoIdentity(r.Context(), account.ID, patch); err != nil {
		util.WriteJSONStatus(w, http.StatusConflict, map[string]string{"error": "account identity changed concurrently; reload and try again"})
		return
	}
	util.WriteJSON(w, map[string]any{"status": "updated", "account_id": account.ID})
}
