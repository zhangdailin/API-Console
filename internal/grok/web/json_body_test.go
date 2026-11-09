package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/testutil"
)

// The Web plane decodes request bodies with the shared httpserver decoder. A
// body is exactly one JSON value: a valid prefix followed by a second value is
// malformed, not a request whose tail is silently dropped.
func TestWebJSONEndpointsRejectTrailingJSONValue(t *testing.T) {
	h := &Handler{}
	for name, handle := range map[string]http.HandlerFunc{
		"chat":   h.HandleChatCompletions,
		"images": h.HandleImagesGenerations,
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/grok/v1/x", strings.NewReader(`{"model":"grok-4"} {"model":"other"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handle(rec, req)
			testutil.Equal(t, rec.Code, http.StatusBadRequest)
			testutil.MustContain(t, rec.Body.String(), `"invalid json"`)
		})
	}
}
