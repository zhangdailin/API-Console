package workbuddy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"orchids-api/internal/store"
	"testing"
	"time"
)

func TestCatalogAndBillingUseDesktopIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v3/config":
			if got := r.UserAgent(); got != "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0" {
				t.Errorf("catalog UA = %q", got)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[{"id":"test-model"}]}}`))
		case "/v2/billing/meter/get-user-resource":
			if got := r.UserAgent(); got != "WorkBuddy/5.8.4" {
				t.Errorf("billing UA = %q", got)
			}
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Error("billing credential lost")
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client := NewFromAccount(&store.Account{WorkBuddyAccessToken: "access", WorkBuddyUID: "uid", WorkBuddyExpiresAt: time.Now().Add(time.Hour)}, nil)
	client.baseURL = server.URL
	client.httpClient = server.Client()
	if _, err := client.FetchModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.FetchQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
}
