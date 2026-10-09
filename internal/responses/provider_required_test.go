package responses

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBridgesRejectRetiredPathsBeforeStorageOrChat(t *testing.T) {
	for _, build := range []func(http.HandlerFunc, BridgeOptions) http.HandlerFunc{ResponsesBridgeHandler, ResponsesBridgeCompactHandler} {
		handler := build(func(http.ResponseWriter, *http.Request) { t.Fatal("called chat on retired path") }, BridgeOptions{})
		for _, path := range []string{"/responses", "/v1/responses", "/v1/responses/compact", "/unknown/v1/responses"} {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"m","input":"hi","previous_response_id":"old"}`)))
			if rec.Code != 404 {
				t.Fatalf("%s: %d", path, rec.Code)
			}
		}
	}
}
