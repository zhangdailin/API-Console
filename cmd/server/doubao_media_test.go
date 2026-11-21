package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"orchids-api/internal/config"
)

func TestDoubaoMediaRoutesStayBehindChannelAuthentication(t *testing.T) {
	e := newChannelE2E(t, "doubao-media-routes:", "sk-doubao-media-e2e", &config.Config{AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin"})
	for _, prefix := range []string{"/doubao", "/doubao/v1"} {
		for _, path := range []string{"/files", "/images/upload", "/images/generations", "/images/edits", "/video/generations"} {
			r := httptest.NewRequest(http.MethodPost, prefix+path, strings.NewReader("{}"))
			w := httptest.NewRecorder()
			e.mux.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: unauthenticated status %d, want 401", prefix+path, w.Code)
			}
			response := e.do(t, http.MethodPost, prefix+path, "{}", false)
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s: authenticated status %d, want 400; %s", prefix+path, response.StatusCode, e.readBody(t, response))
			}
			response.Body.Close()
		}
	}
	for _, path := range []string{"/v1/images/generations", "/doubao/v1/messages", "/doubao/v1/responses"} {
		w := httptest.NewRecorder()
		e.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")))
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: unexpected status %d", path, w.Code)
		}
	}
}

func TestDoubaoCookieImportRequiresAdminAndRedactsCredential(t *testing.T) {
	const cookie = "synthetic-cookie-not-for-output"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/passport/account/info/v2/" {
			http.NotFound(w, r)
			return
		}
		if value, err := r.Cookie("sessionid"); err != nil || value.Value != cookie {
			t.Error("the verified import did not use the submitted session")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"user_id_str":"doubao-import-user","name":"Test account"}}`)
	}))
	defer upstream.Close()
	e := newChannelE2E(t, "doubao-cookie-routes:", "sk-doubao-cookie-e2e", &config.Config{
		AdminUser: "admin", AdminPass: "secret", AdminToken: "admintoken", AdminPath: "/admin", DoubaoBaseURL: upstream.URL,
	})
	const path = "/api/doubao/cookie-import"
	input := `{"cookie":"sessionid=` + cookie + `","enabled":false}`
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(input)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected anonymous status %d", w.Code)
	}
	for _, status := range []int{http.StatusCreated, http.StatusConflict} {
		r := e.do(t, http.MethodPost, path, input, true)
		response := e.readBody(t, r)
		if r.StatusCode != status || strings.Contains(response, cookie) {
			t.Fatalf("wanted status %d and no credential; got %d: %s", status, r.StatusCode, response)
		}
	}
}
