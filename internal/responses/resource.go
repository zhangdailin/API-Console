package responses

import (
	"net/http"
	"strings"

	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// ResourceHandler retrieves or deletes a stored response. Records are
// served from the bridge's store, which is the shared Redis store when one is
// configured and the in-process fallback otherwise. A miss answers with the
// Responses response_not_found envelope instead of Go's plain-text 404, so a
// client can tell "not stored here" from "no such route".
//
// The path is parsed before the method so the sibling endpoints below a response
// id (/cancel, /input_items) never look like a response id themselves.
func ResourceHandler(opts BridgeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if action := subResourceAction(r.URL.Path); action != "" {
			subResourceHandler(action, opts)(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			w.Header().Set("Allow", "GET, DELETE")
			WriteAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
			return
		}
		responseID := ResponseIDFromResourcePath(r.URL.Path)
		if responseID == "" {
			WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "response_id is required")
			return
		}
		st := opts.StoreFor()
		owner := OwnerHash(r.Context())
		record, err := st.GetStoredResponse(r.Context(), responseID, owner)
		if err != nil {
			WriteStoredLookupError(w, err, "response not found")
			return
		}
		ServeStoredResource(w, r, record, st)
	}
}

// ServeStoredResource serves a GET or DELETE after the caller has checked the
// method and loaded the record with ownership enforcement. Native providers
// can use this for bridge records without repeating the store lookup.
func ServeStoredResource(w http.ResponseWriter, r *http.Request, record *store.StoredResponse, st Store) {
	if r.Method == http.MethodDelete {
		if err := st.DeleteStoredResponse(r.Context(), record.ResponseID, OwnerHash(r.Context())); err != nil {
			WriteAPIError(w, http.StatusServiceUnavailable, "response_store_unavailable", "failed to delete response")
			return
		}
		util.WriteJSON(w, map[string]interface{}{"id": record.ResponseID, "object": "response.deleted", "deleted": true})
		return
	}
	if len(record.Body) == 0 {
		WriteAPIError(w, http.StatusNotFound, "response_not_found", "response not found")
		return
	}
	contentType := strings.TrimSpace(record.ContentType)
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(record.Body)
}
