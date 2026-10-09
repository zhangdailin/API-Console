package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedInferenceRejectsMissingProviderBeforeAccess(t *testing.T) {
	// A nil handler proves the rejection occurs before configuration/storage.
	var h *Handler
	for _, path := range []string{"/messages", "/v1/messages", "/chat/completions", "/v1/chat/completions", "/unknown/v1/messages"} {
		rec := httptest.NewRecorder()
		h.HandleMessages(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m"}`)))
		if rec.Code != 404 || !strings.Contains(rec.Body.String(), `"error"`) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
}
