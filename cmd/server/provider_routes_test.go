package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/channel"
	"orchids-api/internal/config"
	"orchids-api/internal/store"
)

func TestProviderRoutesBothBasesAndRetiredUnifiedRoutes(t *testing.T) {
	cfg := &config.Config{AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin"}
	mux, s, _ := newRouteMux(t, "provider-bases:", cfg)
	managedKey := "sk-provider-bases"
	digest := sha256.Sum256([]byte(managedKey))
	if err := s.CreateApiKey(context.Background(), &store.ApiKey{Name: "test", KeyHash: hex.EncodeToString(digest[:]), Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, d := range channel.All() {
		if err := s.CreateModel(context.Background(), &store.Model{Channel: string(d.ID), ModelID: "shared-model", Name: "shared-model", Status: store.ModelStatusAvailable, Verified: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range channel.All() {
		for _, base := range channel.PrefixesFor(d.ID) {
			t.Run(base, func(t *testing.T) {
				call := func(method, path, body, key string) *httptest.ResponseRecorder {
					r := httptest.NewRequest(method, path, strings.NewReader(body))
					if key != "" {
						r.Header.Set("Authorization", "Bearer "+key)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					return w
				}
				for _, endpoint := range []string{"/models", "/models/shared-model", "/messages", "/messages/count_tokens", "/chat/completions", "/responses", "/responses/compact", "/responses/missing", "/responses/missing/cancel", "/responses/missing/input_items"} {
					if w := call(http.MethodPost, base+endpoint, "{}", ""); w.Code != http.StatusUnauthorized {
						t.Fatalf("%s missing key = %d %s", endpoint, w.Code, w.Body.String())
					}
					method := http.MethodGet
					if strings.HasPrefix(endpoint, "/models") || endpoint == "/responses/missing" || strings.HasSuffix(endpoint, "/input_items") {
						method = http.MethodPut
					}
					if w := call(method, base+endpoint, "{}", managedKey); w.Code != http.StatusMethodNotAllowed {
						t.Fatalf("%s registered method guard = %d %s", endpoint, w.Code, w.Body.String())
					}
				}
				for _, endpoint := range []string{"/models", "/models/shared-model"} {
					w := call(http.MethodGet, base+endpoint, "", managedKey)
					if w.Code != 200 || !strings.Contains(w.Body.String(), `"owned_by":"`+string(d.ID)+`"`) {
						t.Fatalf("model provider lost: %d %s", w.Code, w.Body.String())
					}
					var body map[string]interface{}
					if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
						t.Fatal(err)
					}
					if rows, ok := body["data"].([]interface{}); ok && len(rows) != 1 {
						t.Fatalf("cross-provider catalog: %s", w.Body.String())
					}
				}
				w := call(http.MethodPost, base+"/messages/count_tokens", `{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`, managedKey)
				if w.Code != 200 || !strings.Contains(w.Body.String(), `"prompt_profile":"`+string(d.ID)+`"`) {
					t.Fatalf("token profile lost: %d %s", w.Code, w.Body.String())
				}
			})
		}
	}
	for _, prefix := range []string{"", "/v1"} {
		for _, suffix := range []string{"/models", "/models/shared-model", "/messages", "/messages/count_tokens", "/chat/completions", "/responses", "/responses/compact", "/responses/missing", "/responses/missing/cancel", "/responses/missing/input_items"} {
			for _, key := range []string{"", managedKey} {
				for _, method := range []string{http.MethodGet, http.MethodPost} {
					r := httptest.NewRequest(method, prefix+suffix, strings.NewReader(`{"model":"shared-model"}`))
					r.Header.Set("Authorization", "Bearer "+key)
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					if w.Code != 404 || !strings.Contains(w.Body.String(), "404 page not found") {
						t.Fatalf("retired route %s %s = %d %s", method, prefix+suffix, w.Code, w.Body.String())
					}
				}
			}
		}
	}
}
