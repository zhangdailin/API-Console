package console

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
)

const videoPollInterval = 2 * time.Second

// VideoBackend serves Console video routes on the Web plane's video job
// runtime.
type VideoBackend struct{ H *Handler }

// VideoModel accepts a Console route with the video capability.
func (b VideoBackend) VideoModel(ctx context.Context, modelID string) (string, bool) {
	if b.H == nil || !IsConsoleModelID(modelID) {
		return "", false
	}
	return b.H.Route(ctx, modelID, store.CapabilityVideo)
}

// RunVideo creates the video on one Console account, polls it and downloads
// the result.
func (b VideoBackend) RunVideo(ctx context.Context, job web.VideoBackendJob) (web.VideoBackendResult, error) {
	h := b.H
	row, err := h.route(ctx, job.Model, store.CapabilityVideo)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	payload, err := videoPayload(job)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	var requestID string
	_, sess, err := h.Call(ctx, row, func(sess *Session) (*http.Response, error) {
		body, err := h.DoJSON(ctx, sess, http.MethodPost, "/videos/generations", payload)
		if err != nil {
			return nil, err
		}
		if requestID, err = grokbase.ParseVideoCreate(body); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	})
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	defer sess.Close()
	job.Bind(sess.Account.ID, ProviderConsole, requestID)
	return h.pollVideo(ctx, sess, requestID, job.Progress)
}

// ResumeVideo polls a Console video request created before a restart, on
// the account that created it.
func (b VideoBackend) ResumeVideo(ctx context.Context, job web.VideoBackendJob, accountID int64, requestID string) (web.VideoBackendResult, error) {
	sess, err := b.H.OpenSessionByID(ctx, accountID)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	defer sess.Close()
	return b.H.pollVideo(ctx, sess, requestID, job.Progress)
}

func (h *Handler) pollVideo(ctx context.Context, sess *Session, requestID string, progress func(int)) (web.VideoBackendResult, error) {
	for {
		body, err := h.DoJSON(ctx, sess, http.MethodGet, "/videos/"+url.PathEscape(requestID), nil)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		videoURL, done, err := grokbase.ParseVideoStatus(body, progress)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		if done {
			data, mimeType, err := grokbase.DownloadVideo(ctx, h.consoleClient().HTTPClient(), videoURL)
			if err != nil {
				return web.VideoBackendResult{}, err
			}
			return web.VideoBackendResult{URL: videoURL, Data: data, MimeType: mimeType}, nil
		}
		timer := time.NewTimer(videoPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return web.VideoBackendResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// videoPayload is the xAI video request: reference images stay in
// reference_images (image would make the first one a first frame).
func videoPayload(job web.VideoBackendJob) (map[string]any, error) {
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
		for _, reference := range job.InputReferences {
			references = append(references, map[string]any{"url": strings.TrimSpace(reference)})
		}
		payload["reference_images"] = references
	}
	return payload, nil
}
