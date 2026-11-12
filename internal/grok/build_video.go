package grok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
)

// Build video: a Build OAuth account whose own catalog lists a video model
// (grok-imagine-video-1.5 on paid plans) serves it through the CLI proxy's
// /videos/generations endpoint. The route exists only as a discovered catalog
// row, so an account pool that never observed the model serves nothing. Jobs
// run on the Web plane's video job runtime through web.VideoBackend.

const (
	buildVideoPollInterval   = 2 * time.Second
	maxBuildVideoStatusBytes = 2 << 20
	maxBuildVideoAssetBytes  = 512 << 20
	maxBuildVideoInlineBytes = 32 << 20
	buildVideoMessageLimit   = 160
)

// buildVideoBackend adapts the Build plane to web.VideoBackend.
type buildVideoBackend struct{ h *Handler }

// VideoModel accepts a model only when its catalog row is a Build video route.
func (b buildVideoBackend) VideoModel(ctx context.Context, modelID string) (string, bool) {
	h := b.h
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return "", false
	}
	id := strings.TrimPrefix(normalizeModelID(modelID), "build/")
	if id == "" || web.IsWebModelID(id) {
		return "", false
	}
	row, err := h.lb.Store.GetModelByChannelAndModelID(ctx, "grok", id)
	if err != nil || row == nil || !strings.EqualFold(strings.TrimSpace(row.Provider), ProviderBuild) || !row.SupportsCapability(store.CapabilityVideo) {
		return "", false
	}
	upstream := strings.TrimSpace(row.UpstreamModel)
	if upstream == "" {
		upstream = id
	}
	return upstream, true
}

// RunVideo creates the video on one Build account, polls it to completion and
// downloads the result.
func (b buildVideoBackend) RunVideo(ctx context.Context, job web.VideoBackendJob) (web.VideoBackendResult, error) {
	h := b.h
	payload, err := buildVideoPayload(job)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	sess, err := h.openCLIAccountSession(ctx, nil, job.UpstreamModel)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	defer sess.Close()
	job.Bind(sess.acc.ID, ProviderBuild, "")

	client := h.buildClient()
	response, err := client.doResponsesUncheckedAt(ctx, sess.acc, "/videos/generations", payload)
	if err != nil {
		h.markAccountStatus(ctx, sess.acc, err)
		return web.VideoBackendResult{}, err
	}
	body, err := readBuildVideoBody(response)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	requestID, err := parseBuildVideoCreate(body)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	job.Bind(0, "", requestID)

	for {
		response, err := client.doResponseResource(ctx, sess.acc, http.MethodGet, "/videos/"+url.PathEscape(requestID), "")
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		body, err := readBuildVideoBody(response)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		videoURL, done, err := parseBuildVideoStatus(body, job.Progress)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		if done {
			data, mimeType, err := client.downloadBuildVideo(ctx, videoURL)
			if err != nil {
				return web.VideoBackendResult{}, err
			}
			return web.VideoBackendResult{URL: videoURL, Data: data, MimeType: mimeType}, nil
		}
		timer := time.NewTimer(buildVideoPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return web.VideoBackendResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// buildVideoPayload shapes the CLI proxy request. Reference images stay in
// reference_images even when there is one: the upstream treats `image` as a
// first frame, which is a different request.
func buildVideoPayload(job web.VideoBackendJob) (map[string]any, error) {
	prompt := strings.TrimSpace(job.Prompt)
	if prompt == "" {
		return nil, errors.New("video prompt is required")
	}
	payload := map[string]any{"model": job.UpstreamModel, "prompt": prompt}
	if job.Seconds > 0 {
		payload["duration"] = job.Seconds
	}
	if ratio := strings.TrimSpace(job.AspectRatio); ratio != "" {
		payload["aspect_ratio"] = ratio
	}
	if resolution := strings.TrimSpace(job.Resolution); resolution != "" {
		payload["resolution"] = resolution
	}
	if len(job.InputReferences) > 0 {
		if strings.EqualFold(strings.TrimSpace(job.Resolution), "1080p") {
			return nil, errors.New("reference image video generation supports at most 720p")
		}
		references := make([]map[string]any, 0, len(job.InputReferences))
		inline := 0
		for _, reference := range job.InputReferences {
			reference = strings.TrimSpace(reference)
			if reference == "" {
				return nil, errors.New("video reference image is empty")
			}
			if strings.HasPrefix(strings.ToLower(reference), "data:") {
				inline += len(reference)
				if inline > maxBuildVideoInlineBytes {
					return nil, errors.New("combined inline reference images exceed 32 MiB")
				}
			}
			references = append(references, map[string]any{"image_url": reference})
		}
		payload["reference_images"] = references
	}
	return payload, nil
}

func readBuildVideoBody(response *http.Response) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("Build video returned an empty response")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBuildVideoStatusBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBuildVideoStatusBytes {
		return nil, errors.New("Build video response exceeds 2 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// The body is not attached: it is upstream prose on a client-visible job.
		return nil, fmt.Errorf("Build video upstream returned status %d", response.StatusCode)
	}
	return body, nil
}

func parseBuildVideoCreate(body []byte) (string, error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", errors.New("Build video create response is not JSON")
	}
	if message := nestedString(root, "error", "message"); message != "" {
		return "", fmt.Errorf("Build video creation failed: %s", boundVideoMessage(message))
	}
	if id := firstStringField(root, "request_id", "id"); id != "" {
		return id, nil
	}
	if data, ok := root["data"].(map[string]any); ok {
		if id := firstStringField(data, "request_id", "id"); id != "" {
			return id, nil
		}
	}
	return "", errors.New("Build video create response has no request_id")
}

// parseBuildVideoStatus reads one poll: the video URL once the job is done, an
// error when the upstream reports failure, otherwise "keep polling".
func parseBuildVideoStatus(body []byte, progress func(int)) (videoURL string, done bool, err error) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return "", false, errors.New("Build video status response is not JSON")
	}
	if message := nestedString(root, "error", "message"); message != "" {
		return "", false, fmt.Errorf("Build video generation failed: %s", boundVideoMessage(message))
	}
	source := root
	if data, ok := root["data"].(map[string]any); ok {
		source = data
	}
	if value, ok := source["progress"].(float64); ok && progress != nil {
		progress(int(value))
	}
	videoURL = buildVideoURL(root)
	switch strings.ToLower(firstStringField(source, "status", "state")) {
	case "failed", "error", "cancelled", "canceled":
		message := nestedString(source, "error", "message")
		if message == "" {
			message = firstStringField(source, "error_message", "message", "status", "state")
		}
		return "", false, fmt.Errorf("Build video generation failed: %s", boundVideoMessage(message))
	case "completed", "succeeded", "success", "ready", "done":
		if videoURL == "" {
			return "", false, errors.New("Build video completed without a content URL")
		}
		return videoURL, true, nil
	default:
		return videoURL, videoURL != "", nil
	}
}

func buildVideoURL(root map[string]any) string {
	candidates := []string{
		firstStringField(root, "video_url", "url"),
		nestedString(root, "video", "url"), nestedString(root, "output", "url"), nestedString(root, "result", "url"),
	}
	collect := func(items []any) {
		for _, item := range items {
			if entry, ok := item.(map[string]any); ok {
				candidates = append(candidates, firstStringField(entry, "url", "video_url"))
			}
		}
	}
	switch data := root["data"].(type) {
	case map[string]any:
		candidates = append(candidates, firstStringField(data, "video_url", "url"), nestedString(data, "video", "url"), nestedString(data, "output", "url"))
		if items, ok := data["data"].([]any); ok {
			collect(items)
		}
	case []any:
		collect(data)
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

func firstStringField(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := values[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func nestedString(values map[string]any, object, field string) string {
	nested, _ := values[object].(map[string]any)
	if nested == nil {
		return ""
	}
	return firstStringField(nested, field)
}

// boundVideoMessage keeps an upstream failure message short: it ends up on a
// job a client reads back.
func boundVideoMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= buildVideoMessageLimit {
		return message
	}
	return strings.ToValidUTF8(message[:buildVideoMessageLimit], "")
}

// trustedBuildVideoURL accepts only https URLs on xAI's own asset hosts.
func trustedBuildVideoURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{"x.ai", "grok.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

// downloadBuildVideo fetches a finished video from xAI's CDN. The asset host
// needs no credential, so none is sent, and redirects must stay on xAI hosts.
func (c *CLIClient) downloadBuildVideo(ctx context.Context, rawURL string) ([]byte, string, error) {
	if c == nil || c.httpClient == nil || !trustedBuildVideoURL(rawURL) {
		return nil, "", errors.New("Build video content URL is not trusted")
	}
	client := *c.httpClient
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if !trustedBuildVideoURL(request.URL.String()) {
			return errors.New("Build video redirected to an untrusted URL")
		}
		if len(via) >= 10 {
			return errors.New("too many Build video redirects")
		}
		return nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "video/*,*/*;q=0.8")
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, "", fmt.Errorf("Build video download returned status %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBuildVideoAssetBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) == 0 || len(raw) > maxBuildVideoAssetBytes {
		return nil, "", errors.New("Build video is empty or exceeds 512 MiB")
	}
	mimeType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if mimeType == "" || mimeType == "application/octet-stream" {
		mimeType = "video/mp4"
	}
	if !strings.HasPrefix(mimeType, "video/") {
		return nil, "", errors.New("Build video has an invalid Content-Type")
	}
	return raw, mimeType, nil
}
