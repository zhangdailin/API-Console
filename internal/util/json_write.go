package util

import (
	"net/http"

	"encoding/json"
)

// WriteJSON writes the same JSON envelope for admin and inference endpoints.
func WriteJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// WriteJSONStatus is WriteJSON with an explicit status, so a caller can answer
// a failure with the body shape every other endpoint uses.
func WriteJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
