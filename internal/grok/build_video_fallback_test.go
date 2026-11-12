package grok

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/config"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// mp4Header is enough for http.DetectContentType to read video/mp4.
var mp4Header = "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom" + strings.Repeat("v", 64)

func TestBuildVideoFallsBackToTheXAIAPIAndReceivesTheUpload(t *testing.T) {
	var h *Handler
	var mu sync.Mutex
	var fallbackBody map[string]any
	uploaded := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/videos/generations":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"forbidden"}`)
		case r.URL.Path == "/xai/videos/generations":
			testutil.CheckEqual(t, r.Header.Get("User-Agent") != "" && strings.HasPrefix(r.Header.Get("User-Agent"), "xai-grok-build/"), true)
			mu.Lock()
			testutil.CheckNoError(t, json.NewDecoder(r.Body).Decode(&fallbackBody))
			mu.Unlock()
			// xAI uploads the finished video to the URL it was handed.
			output, _ := fallbackBody["output"].(map[string]any)
			uploadURL, _ := output["upload_url"].(string)
			parsed, _ := url.Parse(uploadURL)
			rec := httptest.NewRecorder()
			h.HandleVideoUpload(rec, httptest.NewRequest(http.MethodPut, parsed.Path, strings.NewReader(mp4Header)))
			testutil.CheckEqual(t, rec.Code, http.StatusNoContent)
			replay := httptest.NewRecorder()
			h.HandleVideoUpload(replay, httptest.NewRequest(http.MethodPut, parsed.Path, strings.NewReader(mp4Header)))
			testutil.CheckEqual(t, replay.Code, http.StatusConflict)
			mu.Lock()
			uploaded = true
			mu.Unlock()
			_, _ = io.WriteString(w, `{"request_id":"xai_vid"}`)
		case r.URL.Path == "/xai/videos/xai_vid":
			_, _ = io.WriteString(w, `{"status":"done"}`)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	var acc *store.Account
	h, acc = parityBuildHandler(t, server)
	cfg := *h.configSnapshot()
	cfg.GrokBuildVideoUploadBaseURL = "https://gateway.example"
	cfg.GrokBuildFallbackBaseURL = server.URL + "/xai"
	cfg.MediaDir = t.TempDir()
	h.cfg = &cfg
	ctx := context.Background()
	acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-imagine-video-1.5"}, time.Now()
	acc.GrokBilling = store.GrokBillingSnapshot{Monthly: store.GrokQuotaWindow{HasLimit: true, Limit: 100}, SyncedAt: time.Now()}
	testutil.NoError(t, h.lb.Store.CreateAccount(ctx, acc))

	result, err := buildVideoBackend{h: h}.RunVideo(ctx, web.VideoBackendJob{
		Model: "grok-imagine-video-1.5", UpstreamModel: "grok-imagine-video-1.5", Prompt: "dunes", Seconds: 6,
		InputReferences: []string{"https://example.com/r.png"},
		Bind:            func(int64, string, string) {}, Progress: func(int) {},
	})
	testutil.NoError(t, err)
	testutil.True(t, uploaded)
	testutil.Equal(t, string(result.Data), mp4Header)
	testutil.Equal(t, result.MimeType, "video/mp4")
	testutil.Equal(t, fallbackBody["model"], any("grok-imagine-video-1.5-preview"))
	references, _ := fallbackBody["reference_images"].([]any)
	reference, _ := references[0].(map[string]any)
	testutil.Equal(t, reference["url"], any("https://example.com/r.png"))
	output, _ := fallbackBody["output"].(map[string]any)
	testutil.True(t, strings.HasPrefix(output["upload_url"].(string), "https://gateway.example/grok/v1/media/uploads/"))
}

func TestBuildVideoFallbackNeedsConfigurationAndAPaidAccount(t *testing.T) {
	paid := &store.Account{AccountType: "grok", GrokBilling: store.GrokBillingSnapshot{Monthly: store.GrokQuotaWindow{Limit: 100}}}
	free := &store.Account{AccountType: "grok"}
	forbidden := newCLIUpstreamError(http.StatusForbidden, http.Header{}, []byte(`{}`))
	on := fallbackConfigForTest("https://gateway.example")
	testutil.True(t, fallbackEligible(on, paid, forbidden))
	testutil.False(t, fallbackEligible(on, free, forbidden), "a Free account has no xAI video entitlement")
	testutil.False(t, fallbackEligible(fallbackConfigForTest(""), paid, forbidden), "off by default")
	testutil.False(t, fallbackEligible(fallbackConfigForTest("http://gateway.example"), paid, forbidden), "xAI uploads over https only")
	testutil.False(t, fallbackEligible(on, paid, newCLIUpstreamError(http.StatusTooManyRequests, http.Header{}, []byte(`{}`))))
}

func TestVideoUploadReceiverRefusesUnknownTokensAndNonVideo(t *testing.T) {
	h := NewHandler(fallbackConfigForTest("https://gateway.example"), nil)
	h.cfg.MediaDir = t.TempDir()
	rec := httptest.NewRecorder()
	h.HandleVideoUpload(rec, httptest.NewRequest(http.MethodPut, "/grok/v1/media/uploads/"+strings.Repeat("a", 64), strings.NewReader(mp4Header)))
	testutil.Equal(t, rec.Code, http.StatusNotFound)
	rec = httptest.NewRecorder()
	h.HandleVideoUpload(rec, httptest.NewRequest(http.MethodPut, "/grok/v1/media/uploads/short", strings.NewReader(mp4Header)))
	testutil.Equal(t, rec.Code, http.StatusNotFound)

	uploadURL, _, err := h.issueVideoUpload(context.Background(), h.cfg)
	testutil.NoError(t, err)
	parsed, _ := url.Parse(uploadURL)
	rec = httptest.NewRecorder()
	h.HandleVideoUpload(rec, httptest.NewRequest(http.MethodPut, parsed.Path, strings.NewReader("<html>not a video</html>")))
	testutil.Equal(t, rec.Code, http.StatusUnsupportedMediaType)
	// A refused body does not consume the ticket.
	rec = httptest.NewRecorder()
	h.HandleVideoUpload(rec, httptest.NewRequest(http.MethodPut, parsed.Path, strings.NewReader(mp4Header)))
	testutil.Equal(t, rec.Code, http.StatusNoContent)
	rec = httptest.NewRecorder()
	h.HandleVideoUpload(rec, httptest.NewRequest(http.MethodGet, parsed.Path, nil))
	testutil.Equal(t, rec.Code, http.StatusMethodNotAllowed)
}

func fallbackConfigForTest(base string) *config.Config {
	return &config.Config{GrokBuildVideoUploadBaseURL: base}
}
