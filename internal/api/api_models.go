package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

func (a *API) HandleModels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		models, err := a.store.ListModels(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if hasAccountScopedModels(models) {
			accounts, err := a.store.GetEnabledAccounts(r.Context())
			if err != nil {
				http.Error(w, "Failed to read model account entitlements", http.StatusServiceUnavailable)
				return
			}
			models = filterAdminModelEntitlements(models, accounts)
		}
		// A bare array is the long-standing contract the bundled admin UI
		// consumes. Asking for a page switches to the paged envelope that
		// the newer admin client expects, without breaking the old shape.
		page, pageSize, paged := adminModelPaging(r)
		if !paged {
			util.WriteJSON(w, models)
			return
		}
		models = filterAdminModels(models, r.URL.Query().Get("search"))
		items, total := paginateAdminRows(models, page, pageSize)
		util.WriteJSON(w, adminModelListEnvelope{
			Items:    items,
			Page:     page,
			PageSize: pageSize,
			Total:    total,
		})

	case http.MethodPost:
		var m store.Model
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := a.store.CreateModel(r.Context(), &m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		util.WriteJSONStatus(w, http.StatusCreated, m)

	default:
		writeMethodNotAllowed(w)
	}
}

// writeModelStoreError reports a model-store failure. A row that is not there
// is a 404 whether the store says so with its sentinel or with the driver's own
// nil reply; anything else is the store's text.
func writeModelStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNoRows) || err.Error() == "redis: nil" {
		http.Error(w, "Model not found", http.StatusNotFound)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func (a *API) HandleModelByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/models/")
	if id == "" {
		http.Error(w, "Model ID required", http.StatusBadRequest)
		return
	}

	switch r.Method {
	case http.MethodGet:
		m, err := a.store.GetModel(r.Context(), id)
		if err != nil {
			writeModelStoreError(w, err)
			return
		}
		util.WriteJSON(w, m)

	case http.MethodPut:
		var patch store.Model
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Admin edits own presentation and enablement fields only. Discovery owns
		// verification, provider, upstream mapping, capabilities and provenance;
		// replacing the whole row here used to make a verified Grok model vanish
		// from the tools picker immediately after renaming it.
		m, err := a.store.GetModel(r.Context(), id)
		if err != nil {
			http.Error(w, "Model not found", http.StatusNotFound)
			return
		}
		m.Channel = patch.Channel
		m.ModelID = patch.ModelID
		m.Name = patch.Name
		m.Status = patch.Status
		m.IsDefault = patch.IsDefault
		m.SortOrder = patch.SortOrder
		if err := a.store.UpdateModel(r.Context(), m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		util.WriteJSON(w, m)

	case http.MethodDelete:
		if err := a.store.DeleteModel(r.Context(), id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		writeMethodNotAllowed(w)
	}
}
