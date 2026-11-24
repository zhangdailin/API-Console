package responses

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"orchids-api/internal/chatwire"
	"orchids-api/internal/util"
	"strings"

	"encoding/json"

	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

// The Responses API exposes two endpoints below a single response id. Both are
// served from the response store, so they work for every channel the gateway
// bridges rather than only for the one that happens to own the path.
const (
	responsesActionCancel     = "cancel"
	responsesActionInputItems = "input_items"
)

// parseResourcePath splits a path below /responses/ into the response
// id and an optional sibling action. `ok` is false when the trailing section is
// empty or names the Create-compaction operation, which is a request of its own
// rather than a resource, so callers keep their existing handling for it.
func parseResourcePath(path string) (id, action string, ok bool) {
	const marker = "/responses/"
	trimmed := strings.TrimSpace(path)
	index := strings.LastIndex(trimmed, marker)
	if index < 0 {
		return "", "", false
	}
	value := strings.Trim(trimmed[index+len(marker):], "/")
	if value == "" {
		return "", "", false
	}
	parts := strings.Split(value, "/")
	candidate := strings.TrimSpace(parts[0])
	if candidate == "" || strings.EqualFold(candidate, "compact") {
		return "", "", false
	}
	decoded, err := url.PathUnescape(candidate)
	if err != nil {
		return "", "", false
	}
	candidate = strings.TrimSpace(decoded)
	if candidate == "" {
		return "", "", false
	}
	if len(parts) > 1 {
		action = strings.ToLower(strings.TrimSpace(parts[len(parts)-1]))
	}
	return candidate, action, true
}

// SubResourceAction reports which sibling endpoint a path names, or ""
// when the path addresses the response itself (or something unknown, which the
// resource handler turns into its own error).
func SubResourceAction(path string) string {
	_, action, ok := parseResourcePath(path)
	if !ok {
		return ""
	}
	switch action {
	case responsesActionCancel, responsesActionInputItems:
		return action
	}
	return ""
}

// SubResourceHandler returns the handler for one sibling action. Both
// answers come from the store, which is why they are channel-agnostic: a record
// written by any channel is served the same way.
func SubResourceHandler(action string, opts BridgeOptions) http.HandlerFunc {
	switch action {
	case responsesActionCancel:
		return CancelHandler(opts)
	case responsesActionInputItems:
		return InputItemsHandler(opts)
	default:
		return func(w http.ResponseWriter, r *http.Request) {
			WriteAPIError(w, http.StatusNotFound, "not_found", "unknown responses sub-resource")
		}
	}
}

// CancelHandler implements POST /responses/{response_id}/cancel.
func CancelHandler(opts BridgeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeMethodNotAllowed(w, http.MethodPost)
			return
		}
		responseID, _, ok := parseResourcePath(r.URL.Path)
		if !ok {
			WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "response_id is required")
			return
		}
		st := opts.StoreFor()
		record, err := storedResponseMetadata(r.Context(), st, responseID, OwnerHash(r.Context()))
		if err != nil {
			WriteStoredLookupError(w, err, "response not found")
			return
		}
		if len(record.Body) == 0 {
			// Ownership without a body means the response lives upstream (Build).
			// The gateway still knows what it is, so it answers with that instead
			// of pretending the response does not exist.
			writeSyntheticCancelledResponse(w, record)
			return
		}
		writeCancelledRecord(w, r, st, record)
	}
}

// InputItemsHandler implements GET /responses/{response_id}/input_items.
func InputItemsHandler(opts BridgeOptions) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeMethodNotAllowed(w, http.MethodGet)
			return
		}
		responseID, _, ok := parseResourcePath(r.URL.Path)
		if !ok {
			WriteAPIError(w, http.StatusBadRequest, "invalid_request_error", "response_id is required")
			return
		}
		st := opts.StoreFor()
		record, err := st.GetStoredResponse(r.Context(), responseID, OwnerHash(r.Context()))
		if err != nil {
			WriteStoredLookupError(w, err, "response not found")
			return
		}
		items, more, err := storedInputHistory(r.Context(), st, record)
		if err != nil {
			WriteStoredLookupError(w, err, "response not found")
			return
		}
		writeStoredInputItems(w, items, more)
	}
}

// writeCancelledRecord flips a stored response to `cancelled` and echoes the
// response object, which is what the Responses SDK expects from cancel.
//
// Only update the representation that was read; a concurrent delete wins.
func writeCancelledRecord(w http.ResponseWriter, r *http.Request, st Store, record *store.StoredResponse) {
	var response map[string]any
	if err := json.Unmarshal(record.Body, &response); err != nil || response == nil {
		WriteAPIError(w, http.StatusBadGateway, "invalid_stored_response", "stored response is not a JSON object")
		return
	}
	if !strings.EqualFold(chatwire.ParseLooseStringAny(response["status"]), "cancelled") {
		response["status"] = "cancelled"
		markOutputItemsCancelled(response)
		encoded, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			WriteAPIError(w, http.StatusInternalServerError, "server_error", "failed to update response")
			return
		}
		updater, ok := st.(interface {
			UpdateStoredResponseBody(context.Context, *store.StoredResponse, []byte) error
		})
		if !ok {
			WriteStoredLookupError(w, store.ErrStoredResponseChanged, "response not found")
			return
		}
		if err := updater.UpdateStoredResponseBody(r.Context(), record, encoded); err != nil {
			if errors.Is(err, store.ErrStoredResponseChanged) {
				fresh, readErr := storedResponseMetadata(r.Context(), st, record.ResponseID, record.OwnerHash)
				if readErr != nil {
					WriteStoredLookupError(w, readErr, "response not found")
				} else if err := json.Unmarshal(fresh.Body, &response); err == nil && response["status"] == "cancelled" {
					util.WriteJSON(w, response)
				} else {
					WriteAPIError(w, http.StatusConflict, "response_changed", "Response changed while cancelling")
				}
				return
			}
			WriteStoredLookupError(w, err, "response not found")
			return
		}
	}
	util.WriteJSON(w, response)
}

// writeSyntheticCancelledResponse answers cancel for a record whose body lives
// upstream. The upstream resource is never mutated: the gateway did not create
// it, cannot read it, and a cancel it cannot verify is worse than an honest
// object carrying the identity it does know.
func writeSyntheticCancelledResponse(w http.ResponseWriter, record *store.StoredResponse) {
	response := map[string]any{
		"id":     record.ResponseID,
		"object": "response",
		"status": "cancelled",
	}
	if model := strings.TrimSpace(record.Model); model != "" {
		response["model"] = model
	}
	if !record.CreatedAt.IsZero() {
		response["created_at"] = record.CreatedAt.Unix()
	}
	response["output"] = []any{}
	util.WriteJSON(w, response)
}

// markOutputItemsCancelled keeps the item statuses consistent with the response
// status. An item that already finished keeps its own status, so the record
// still says what the upstream actually produced.
func markOutputItemsCancelled(response map[string]any) {
	for _, raw := range InterfaceSlice(response["output"]) {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		switch strings.ToLower(chatwire.ParseLooseStringAny(item["status"])) {
		case "", "in_progress", "queued":
			item["status"] = "cancelled"
		}
	}
}

// writeStoredInputItems serves GET /responses/{id}/input_items from what the
// gateway persisted when it wrote the response. A record written before the
// gateway persisted input items reports an empty list rather than failing: the
// response exists, and the caller can still read its output.
func writeStoredInputItems(w http.ResponseWriter, items []json.RawMessage, more bool) {
	payload := map[string]any{
		"object":   "list",
		"data":     items,
		"has_more": more,
	}
	if len(items) > 0 {
		for key, raw := range map[string]json.RawMessage{"first_id": items[0], "last_id": items[len(items)-1]} {
			var item struct{ ID string }
			if json.Unmarshal(raw, &item) == nil {
				payload[key] = item.ID
			}
		}
	}
	util.WriteJSON(w, payload)
}

const maxInputHistoryTurns = 64

// Legacy rows already contain a history snapshot. Include one such snapshot
// when a delta chain reaches it, then stop rather than repeating its ancestors.
func storedInputHistory(ctx context.Context, st Store, first *store.StoredResponse) ([]json.RawMessage, bool, error) {
	var turns [][]json.RawMessage
	seen := make(map[string]bool)
	size, more := 0, false
	for row := first; ; {
		seen[row.ResponseID] = true
		if len(row.InputItems) > config.InferenceJSONBodyLimit-size || len(turns) >= maxInputHistoryTurns {
			more = true
			break
		}
		var items []json.RawMessage
		if len(row.InputItems) > 0 {
			if err := json.Unmarshal(row.InputItems, &items); err != nil {
				return nil, false, err
			}
		}
		size += len(row.InputItems)
		turns = append(turns, items)
		parent := row.PreviousResponseID
		if !row.InputItemsDelta || parent == "" || seen[parent] {
			break
		}
		if len(turns) >= maxInputHistoryTurns {
			more = true
			break
		}
		next, err := st.GetStoredResponse(ctx, parent, first.OwnerHash)
		if errors.Is(err, store.ErrNoRows) {
			break
		}
		if err != nil {
			return nil, false, err
		}
		if next.Provider != first.Provider || next.AccountID != first.AccountID {
			break
		}
		row = next
	}
	result := []json.RawMessage{}
	for i := len(turns) - 1; i >= 0; i-- {
		result = append(result, turns[i]...)
	}
	return result, more, nil
}

// InputItemsJSON normalizes a Responses `input` into the item array the
// resource endpoint reports.
//
// Every item carries an id and a terminal status because that is the shape the
// official clients round-trip: a caller that reads the list and sends it back as
// the next turn's `input` must not be rejected for fields the gateway elided.
// Items the client already labelled keep their own id, so a replay stays
// byte-stable.
func InputItemsJSON(input any) json.RawMessage {
	items := InputItems(input)
	if len(items) == 0 {
		return nil
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return nil
	}
	return encoded
}

// InputItems normalizes input for storage and replay without modifying the caller.
func InputItems(input any) []any {
	switch value := input.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []any{map[string]any{
			"id":      "msg_" + util.RandomHex(12),
			"type":    "message",
			"role":    "user",
			"status":  "completed",
			"content": []any{map[string]any{"type": "input_text", "text": value}},
		}}
	case []any:
		out := make([]any, 0, len(value))
		for _, raw := range value {
			item, _ := raw.(map[string]any)
			if item == nil {
				continue
			}
			copied := CloneStringInterfaceMap(item)
			if copied == nil {
				continue
			}
			if chatwire.ParseLooseStringAny(copied["id"]) == "" {
				copied["id"] = "item_" + util.RandomHex(12)
			}
			if chatwire.ParseLooseStringAny(copied["status"]) == "" {
				copied["status"] = "completed"
			}
			out = append(out, copied)
		}
		return out
	default:
		return nil
	}
}

// writeMethodNotAllowed answers a wrong method with the Responses
// envelope and the Allow header, so a client that probes the endpoint learns the
// contract instead of receiving Go's plain-text 405.
func writeMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	WriteAPIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
}
