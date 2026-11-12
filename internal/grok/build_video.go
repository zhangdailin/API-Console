package grok

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"orchids-api/internal/grok/console"
	"orchids-api/internal/grok/grokbase"
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
	maxBuildVideoInlineBytes = 32 << 20
)

// videoBackends routes a video job to the plane its model belongs to.
type videoBackends struct {
	build   buildVideoBackend
	console console.VideoBackend
}

func (v videoBackends) VideoModel(ctx context.Context, modelID string) (string, bool) {
	if console.IsConsoleModelID(modelID) {
		return v.console.VideoModel(ctx, modelID)
	}
	return v.build.VideoModel(ctx, modelID)
}

func (v videoBackends) ResumeVideo(ctx context.Context, job web.VideoBackendJob, accountID int64, requestID string) (web.VideoBackendResult, error) {
	if console.IsConsoleModelID(job.Model) {
		return v.console.ResumeVideo(ctx, job, accountID, requestID)
	}
	return v.build.ResumeVideo(ctx, job, accountID, requestID)
}

func (v videoBackends) RunVideo(ctx context.Context, job web.VideoBackendJob) (web.VideoBackendResult, error) {
	if console.IsConsoleModelID(job.Model) {
		return v.console.RunVideo(ctx, job)
	}
	return v.build.RunVideo(ctx, job)
}

// buildVideoBackend adapts the Build plane to web.VideoBackend.
type buildVideoBackend struct{ h *Handler }

// VideoModel accepts a model only when its catalog row is a Build video route.
func (b buildVideoBackend) VideoModel(ctx context.Context, modelID string) (string, bool) {
	h := b.h
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return "", false
	}
	id := strings.TrimPrefix(normalizeModelID(modelID), "build/")
	if id == "" || web.IsWebModelID(id) || console.IsConsoleModelID(id) {
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
		if fallbackEligible(h.configSnapshot(), sess.acc, err) {
			return b.runFallbackVideo(ctx, job, sess.acc)
		}
		h.markAccountStatus(ctx, sess.acc, err)
		return web.VideoBackendResult{}, err
	}
	body, err := grokbase.ReadVideoBody(response)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	requestID, err := grokbase.ParseVideoCreate(body)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	job.Bind(0, "", requestID)
	return pollBuildVideo(ctx, client, sess.acc, requestID, job.Progress)
}

// ResumeVideo polls a Build video request created before a restart, on the
// account that created it.
func (b buildVideoBackend) ResumeVideo(ctx context.Context, job web.VideoBackendJob, accountID int64, requestID string) (web.VideoBackendResult, error) {
	sess, err := b.h.openCLIAccountSessionByID(ctx, accountID, job.UpstreamModel)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	defer sess.Close()
	return pollBuildVideo(ctx, b.h.buildClient(), sess.acc, requestID, job.Progress)
}

// pollBuildVideo polls one Build video request to completion and downloads it.
func pollBuildVideo(ctx context.Context, client *CLIClient, acc *store.Account, requestID string, progress func(int)) (web.VideoBackendResult, error) {
	for {
		response, err := client.doResponseResource(ctx, acc, http.MethodGet, "/videos/"+url.PathEscape(requestID), "")
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		body, err := grokbase.ReadVideoBody(response)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		videoURL, done, err := grokbase.ParseVideoStatus(body, progress)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		if done {
			data, mimeType, err := grokbase.DownloadVideo(ctx, client.httpClient, videoURL)
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
