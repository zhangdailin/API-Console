package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"orchids-api/internal/testutil"
)

func TestToolsLinksRewriteResourceLinksInTextResponses(t *testing.T) {
	handler := withToolsLinks(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The video link is split across two writes, as a stream would split it.
		_, _ = w.Write([]byte(`{"content_url":"/grok/v1/vid`))
		_, _ = w.Write([]byte(`eos/v_1/content","url":"https://gw.example/grok/v1/files/image/a.jpg"}`))
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/grok/tools/v1/videos/v_1", nil))
	testutil.Equal(t, rec.Body.String(), `{"content_url":"/api/grok/tools/v1/videos/v_1/content","url":"https://gw.example/api/grok/tools/v1/files/image/a.jpg"}`)
}

func TestToolsLinksLeaveBinaryBodiesUntouched(t *testing.T) {
	payload := []byte("binary /grok/v1/files/ bytes")
	handler := withToolsLinks(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(payload)
	})
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/api/grok/tools/v1/videos/v_1/content", nil))
	testutil.Equal(t, rec.Body.String(), string(payload))
}
