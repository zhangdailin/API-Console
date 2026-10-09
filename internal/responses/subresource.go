package responses

import (
	"net/http"
	"net/url"
	"orchids-api/internal/util"
	"strings"
	"time"

	"encoding/json"

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

// subResourceAction reports which sibling endpoint a path names, or ""
// when the path addresses the response itself (or something unknown, which the
// resource handler turns into its own error).
func subResourceAction(path string) string {
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

// subResourceHandler returns the handler for one sibling action. Both
// answers come from the store, which is why they are channel-agnostic: a record
// written by any channel is served the same way.
func subResourceHandler(action string, opts BridgeOptions) http.HandlerFunc {
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
		record, err := st.GetStoredResponse(r.Context(), responseID, OwnerHash(r.Context()))
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
		writeCancelledRecord(w, r, st, record, opts.TTLOrDefault())
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
		writeStoredInputItems(w, record)
	}
}

// writeCancelledRecord flips a stored response to `cancelled` and echoes the
// response object, which is what the Responses SDK expects from cancel.
//
// Every response the gateway stores is written once it is terminal, so a cancel
// that arrives after the fact changes nothing and is answered with the record as
// it stands. That is what makes the endpoint idempotent instead of an error: a
// client that cancels defensively must not be told its response does not exist.
func writeCancelledRecord(w http.ResponseWriter, r *http.Request, st Store, record *store.StoredResponse, fallbackTTL time.Duration) {
	var response map[string]interface{}
	if err := json.Unmarshal(record.Body, &response); err != nil || response == nil {
		WriteAPIError(w, http.StatusBadGateway, "invalid_stored_response", "stored response is not a JSON object")
		return
	}
	if !strings.EqualFold(ParseLooseStringAny(response["status"]), "cancelled") {
		response["status"] = "cancelled"
		markOutputItemsCancelled(response)
		encoded, encodeErr := json.Marshal(response)
		if encodeErr != nil {
			WriteAPIError(w, http.StatusInternalServerError, "server_error", "failed to update response")
			return
		}
		updated := *record
		updated.Body = encoded
		updated.ContentType = util.FirstNonEmpty(strings.TrimSpace(record.ContentType), "application/json")
		ttl := time.Until(record.ExpiresAt)
		if ttl <= 0 {
			ttl = fallbackTTL
		}
		if err := st.SaveStoredResponse(r.Context(), &updated, ttl); err != nil {
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
	response := map[string]interface{}{
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
	response["output"] = []interface{}{}
	util.WriteJSON(w, response)
}

// markOutputItemsCancelled keeps the item statuses consistent with the response
// status. An item that already finished keeps its own status, so the record
// still says what the upstream actually produced.
func markOutputItemsCancelled(response map[string]interface{}) {
	for _, raw := range InterfaceSlice(response["output"]) {
		item, _ := raw.(map[string]interface{})
		if item == nil {
			continue
		}
		switch strings.ToLower(ParseLooseStringAny(item["status"])) {
		case "", "in_progress", "queued":
			item["status"] = "cancelled"
		}
	}
}

// writeStoredInputItems serves GET /responses/{id}/input_items from what the
// gateway persisted when it wrote the response. A record written before the
// gateway persisted input items reports an empty list rather than failing: the
// response exists, and the caller can still read its output.
func writeStoredInputItems(w http.ResponseWriter, record *store.StoredResponse) {
	items := []interface{}{}
	if len(record.InputItems) > 0 {
		var decoded []interface{}
		if err := json.Unmarshal(record.InputItems, &decoded); err == nil {
			items = decoded
		}
	}
	payload := map[string]interface{}{
		"object":   "list",
		"data":     items,
		"has_more": false,
	}
	if len(items) > 0 {
		if first, ok := items[0].(map[string]interface{}); ok {
			payload["first_id"] = ParseLooseStringAny(first["id"])
		}
		if last, ok := items[len(items)-1].(map[string]interface{}); ok {
			payload["last_id"] = ParseLooseStringAny(last["id"])
		}
	}
	util.WriteJSON(w, payload)
}

// inputItemsJSON normalizes a Responses `input` into the item array the
// resource endpoint reports.
//
// Every item carries an id and a terminal status because that is the shape the
// official clients round-trip: a caller that reads the list and sends it back as
// the next turn's `input` must not be rejected for fields the gateway elided.
// Items the client already labelled keep their own id, so a replay stays
// byte-stable.
func inputItemsJSON(input interface{}) json.RawMessage {
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
func InputItems(input interface{}) []interface{} {
	switch value := input.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil
		}
		return []interface{}{map[string]interface{}{
			"id":      "msg_" + util.RandomHex(12),
			"type":    "message",
			"role":    "user",
			"status":  "completed",
			"content": []interface{}{map[string]interface{}{"type": "input_text", "text": value}},
		}}
	case []interface{}:
		out := make([]interface{}, 0, len(value))
		for _, raw := range value {
			item, _ := raw.(map[string]interface{})
			if item == nil {
				continue
			}
			copied := CloneStringInterfaceMap(item)
			if copied == nil {
				continue
			}
			if ParseLooseStringAny(copied["id"]) == "" {
				copied["id"] = "item_" + util.RandomHex(12)
			}
			if ParseLooseStringAny(copied["status"]) == "" {
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

// SubResourceAction reads the sub-resource action ("cancel", "input_items")
// out of a resource path.
func SubResourceAction(path string) string { return subResourceAction(path) }

// SubResourceHandler serves one sub-resource action.
func SubResourceHandler(action string, opts BridgeOptions) http.HandlerFunc {
	return subResourceHandler(action, opts)
}

// InputItemsJSON renders a request's input as the stored input-item list.
func InputItemsJSON(input interface{}) json.RawMessage { return inputItemsJSON(input) }

// ParseResourcePath splits a path below /responses/ into the response id and
// an optional sibling action.
func ParseResourcePath(path string) (id, action string, ok bool) { return parseResourcePath(path) }

// Action names the two sub-resource endpoints below a response id.
const (
	ActionCancel     = responsesActionCancel
	ActionInputItems = responsesActionInputItems
)
