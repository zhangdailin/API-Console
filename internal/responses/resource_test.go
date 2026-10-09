package responses

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

type deleteFailureStore struct{ *store.MemoryResponseStore }

func (deleteFailureStore) DeleteStoredResponse(context.Context, string, string) error {
	return errors.New("private backend failure")
}

func TestResourceDeleteFailureDoesNotReportSuccess(t *testing.T) {
	s := deleteFailureStore{store.NewMemoryResponseStore(DefaultStoredResponseTTL)}
	if err := s.SaveStoredResponse(context.Background(), &store.StoredResponse{ResponseID: "resp_1", OwnerHash: "anonymous", Body: []byte(`{"id":"resp_1"}`)}, DefaultStoredResponseTTL); err != nil {
		t.Fatal(err)
	}
	resource := ResourceHandler(BridgeOptions{Store: s})
	for _, base := range []string{"/workbuddy", "/workbuddy/v1"} {
		w := httptest.NewRecorder()
		resource(w, httptest.NewRequest(http.MethodDelete, base+"/responses/resp_1", nil))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"response_store_unavailable"`) || strings.Contains(w.Body.String(), "private backend") {
			t.Fatalf("delete failure = %d %s", w.Code, w.Body.String())
		}
		get := httptest.NewRecorder()
		resource(get, httptest.NewRequest(http.MethodGet, base+"/responses/resp_1", nil))
		if get.Code != http.StatusOK {
			t.Fatalf("failed deletion lost record: %d %s", get.Code, get.Body.String())
		}
	}
}
