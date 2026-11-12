package grok

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
	"orchids-api/internal/testutil"
)

// xaiHostRewriter sends requests for xAI asset hosts to the test server, so the
// trusted-host rule is exercised with a real https://*.x.ai URL.
type xaiHostRewriter struct {
	target *url.URL
	next   http.RoundTripper
}

func (r xaiHostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.HasSuffix(req.URL.Hostname(), ".x.ai") {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = r.target.Scheme, r.target.Host
		return r.next.RoundTrip(clone)
	}
	return r.next.RoundTrip(req)
}

func TestBuildVideoRunsCreatePollAndDownloadOnAnObservedRoute(t *testing.T) {
	var mu sync.Mutex
	var created map[string]any
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/videos/generations":
			testutil.CheckNoError(t, json.NewDecoder(r.Body).Decode(&created))
			testutil.CheckEqual(t, strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "), true)
			_, _ = io.WriteString(w, `{"request_id":"vid_req_1"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/videos/vid_req_1":
			polls++
			if polls == 1 {
				_, _ = io.WriteString(w, `{"status":"pending","progress":40}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://videos.x.ai/out/clip.mp4"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/out/clip.mp4":
			// The CDN needs no credential and must not receive one.
			testutil.CheckEqual(t, r.Header.Get("Authorization"), "")
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = io.WriteString(w, "mp4-bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	target, _ := url.Parse(server.URL)
	h.cliClient.httpClient = &http.Client{Transport: xaiHostRewriter{target: target, next: server.Client().Transport}}
	ctx := context.Background()
	acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-imagine-video-1.5"}, time.Now()
	testutil.NoError(t, h.lb.Store.CreateAccount(ctx, acc))

	backend := buildVideoBackend{h: h}
	// No catalog row yet: the route is not served.
	_, ok := backend.VideoModel(ctx, "grok-imagine-video-1.5")
	testutil.False(t, ok)
	testutil.NoError(t, h.lb.Store.CreateModel(ctx, &store.Model{Channel: "grok", ModelID: "grok-imagine-video-1.5", Name: "Video", Status: store.ModelStatusAvailable, Verified: true,
		Provider: ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capabilities: []string{store.CapabilityVideo}}))
	upstream, ok := backend.VideoModel(ctx, "build/grok-imagine-video-1.5")
	testutil.True(t, ok)
	testutil.Equal(t, upstream, "grok-imagine-video-1.5")
	// A Web video route is never the Build backend's.
	_, ok = backend.VideoModel(ctx, "grok-imagine-video")
	testutil.False(t, ok)

	var boundAccount int64
	var boundRequest string
	var progress []int
	result, err := backend.RunVideo(ctx, web.VideoBackendJob{
		ID: "video_test", Model: "grok-imagine-video-1.5", UpstreamModel: upstream, Prompt: "a red kite", Seconds: 6,
		AspectRatio: "16:9", Resolution: "720p", InputReferences: []string{"https://example.com/ref.png"},
		Bind: func(accountID int64, provider, requestID string) {
			if accountID != 0 {
				boundAccount = accountID
				testutil.CheckEqual(t, provider, ProviderBuild)
			}
			if requestID != "" {
				boundRequest = requestID
			}
		},
		Progress: func(p int) { progress = append(progress, p) },
	})
	testutil.NoError(t, err)
	testutil.Equal(t, string(result.Data), "mp4-bytes")
	testutil.Equal(t, result.MimeType, "video/mp4")
	testutil.Equal(t, boundAccount, acc.ID)
	testutil.Equal(t, boundRequest, "vid_req_1")
	testutil.Equal(t, len(progress), 1)
	testutil.Equal(t, created["model"], any("grok-imagine-video-1.5"))
	testutil.Equal(t, created["prompt"], any("a red kite"))
	testutil.Equal(t, created["duration"], any(float64(6)))
	testutil.Equal(t, created["resolution"], any("720p"))
	references, _ := created["reference_images"].([]any)
	testutil.Equal(t, len(references), 1)
	_, hasImage := created["image"]
	testutil.False(t, hasImage, "a single reference is not a first frame")
}

func TestBuildVideoJobThroughTheVideosEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/videos/generations":
			_, _ = io.WriteString(w, `{"id":"vid_req_2"}`)
		case r.URL.Path == "/v1/videos/vid_req_2":
			_, _ = io.WriteString(w, `{"data":{"status":"completed","video_url":"https://cdn.x.ai/a.mp4"}}`)
		case r.URL.Path == "/a.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = io.WriteString(w, "clip")
		}
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	target, _ := url.Parse(server.URL)
	h.cliClient.httpClient = &http.Client{Transport: xaiHostRewriter{target: target, next: server.Client().Transport}}
	ctx := context.Background()
	acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-imagine-video-1.5"}, time.Now()
	testutil.NoError(t, h.lb.Store.CreateAccount(ctx, acc))
	testutil.NoError(t, h.lb.Store.CreateModel(ctx, &store.Model{Channel: "grok", ModelID: "grok-imagine-video-1.5", Name: "Video", Status: store.ModelStatusAvailable, Verified: true,
		Provider: ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capabilities: []string{store.CapabilityVideo}}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/grok/v1/videos", strings.NewReader(`{"model":"grok-imagine-video-1.5","prompt":"waves","seconds":6}`))
	req.Header.Set("Content-Type", "application/json")
	h.Web().HandleVideosCreate(rec, req)
	testutil.Equal(t, rec.Code, http.StatusOK)
	var created map[string]any
	testutil.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	id, _ := created["id"].(string)
	testutil.True(t, strings.HasPrefix(id, "video_"))

	var job *store.StoredVideoJob
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		stored, err := h.lb.Store.GetStoredVideoJob(ctx, id, "anonymous")
		if err == nil && stored != nil && (stored.Status == "completed" || stored.Status == "failed") {
			job = stored
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	testutil.True(t, job != nil, "the job reaches a terminal state")
	testutil.Equal(t, job.Status, "completed")
	testutil.Equal(t, job.Provider, ProviderBuild)
	testutil.Equal(t, job.AccountID, acc.ID)
	testutil.Equal(t, job.UpstreamRequestID, "vid_req_2")
	t.Cleanup(func() { _ = os.Remove(job.ContentPath) })
	raw, err := os.ReadFile(job.ContentPath)
	testutil.NoError(t, err)
	testutil.Equal(t, string(raw), "clip")
}

func TestBuildVideoStatusAndURLRules(t *testing.T) {
	_, done, err := grokbase.ParseVideoStatus([]byte(`{"status":"failed","error_message":"moderated"}`), nil)
	testutil.False(t, done)
	testutil.MustContain(t, err.Error(), "moderated")
	_, _, err = grokbase.ParseVideoStatus([]byte(`{"status":"completed"}`), nil)
	testutil.True(t, err != nil, "done without a URL is a failure")
	_, done, err = grokbase.ParseVideoStatus([]byte(`{"status":"queued"}`), nil)
	testutil.NoError(t, err)
	testutil.False(t, done)

	testutil.True(t, grokbase.TrustedVideoURL("https://videos.x.ai/a.mp4"))
	testutil.True(t, grokbase.TrustedVideoURL("https://assets.grok.com/a.mp4"))
	testutil.False(t, grokbase.TrustedVideoURL("http://videos.x.ai/a.mp4"))
	testutil.False(t, grokbase.TrustedVideoURL("https://x.ai.evil.example/a.mp4"))

	_, err = buildVideoPayload(web.VideoBackendJob{UpstreamModel: "m", Prompt: "p", Resolution: "1080p", InputReferences: []string{"https://e.com/a.png"}})
	testutil.True(t, err != nil)
}

func TestInterruptedVideoJobsAreResumedOrFailedButLeasedOnesAreLeftAlone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/videos/vid_resume":
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://videos.x.ai/r.mp4"}}`)
		case "/r.mp4":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = io.WriteString(w, "resumed")
		default:
			t.Errorf("unexpected upstream call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h, acc := parityBuildHandler(t, server)
	target, _ := url.Parse(server.URL)
	h.cliClient.httpClient = &http.Client{Transport: xaiHostRewriter{target: target, next: server.Client().Transport}}
	ctx := context.Background()
	st := h.lb.Store
	acc.ID, acc.GrokModels, acc.GrokModelsSyncedAt = 0, []string{"grok-imagine-video-1.5"}, time.Now()
	testutil.NoError(t, st.CreateAccount(ctx, acc))
	testutil.NoError(t, st.CreateModel(ctx, &store.Model{Channel: "grok", ModelID: "grok-imagine-video-1.5", Name: "Video", Status: store.ModelStatusAvailable, Verified: true,
		Provider: ProviderBuild, UpstreamModel: "grok-imagine-video-1.5", Capabilities: []string{store.CapabilityVideo}}))

	old := time.Now().Add(-10 * time.Minute).Unix()
	save := func(job *store.StoredVideoJob) {
		job.OwnerHash, job.CreatedAt = "owner", old
		testutil.NoError(t, st.SaveStoredVideoJob(ctx, job, time.Hour))
	}
	save(&store.StoredVideoJob{ID: "video_resume", Model: "grok-imagine-video-1.5", Provider: ProviderBuild, AccountID: acc.ID, UpstreamRequestID: "vid_resume", Status: "in_progress"})
	save(&store.StoredVideoJob{ID: "video_web", Model: "grok-imagine-video", Provider: "web", Status: "in_progress"})
	save(&store.StoredVideoJob{ID: "video_alive", Model: "grok-imagine-video", Provider: "web", Status: "in_progress"})
	held, err := st.AcquireVideoJobLease(ctx, "video_alive", "owner", "another-instance", time.Minute)
	testutil.NoError(t, err)
	testutil.True(t, held)
	// A fresh job is its creator's to lease, not the sweep's.
	testutil.NoError(t, st.SaveStoredVideoJob(ctx, &store.StoredVideoJob{ID: "video_new", OwnerHash: "owner", Model: "grok-imagine-video", Status: "queued", CreatedAt: time.Now().Unix()}, time.Hour))

	h.Web().RecoverVideoJobs(ctx)
	status := func(id string) *store.StoredVideoJob {
		job, err := st.GetStoredVideoJob(ctx, id, "owner")
		testutil.NoError(t, err)
		return job
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (status("video_resume").Status != "completed" || status("video_web").Status != "failed") {
		time.Sleep(50 * time.Millisecond)
	}
	resumed := status("video_resume")
	testutil.Equal(t, resumed.Status, "completed")
	t.Cleanup(func() { _ = os.Remove(resumed.ContentPath) })
	raw, err := os.ReadFile(resumed.ContentPath)
	testutil.NoError(t, err)
	testutil.Equal(t, string(raw), "resumed")
	interrupted := status("video_web")
	testutil.Equal(t, interrupted.Status, "failed")
	testutil.Equal(t, interrupted.ErrorCode, "video_interrupted")
	testutil.Equal(t, status("video_alive").Status, "in_progress")
	testutil.Equal(t, status("video_new").Status, "queued")
}
