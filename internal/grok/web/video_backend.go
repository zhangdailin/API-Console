package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

// The video job runtime (ids, ownership, persistence, the content endpoint)
// lives in this package. A video route of another plane of the grok channel
// plugs its upstream in through VideoBackend and reuses all of it.

// VideoBackend serves the video routes of another plane.
type VideoBackend interface {
	// VideoModel reports whether modelID is one of the backend's observed
	// video routes, and names the upstream model it maps to.
	VideoModel(ctx context.Context, modelID string) (upstreamModel string, ok bool)
	// RunVideo creates the video upstream, waits for it and returns its bytes.
	RunVideo(ctx context.Context, job VideoBackendJob) (VideoBackendResult, error)
}

// VideoBackendJob is one validated video request handed to a backend.
type VideoBackendJob struct {
	ID            string
	Model         string
	UpstreamModel string
	Prompt        string
	Seconds       int
	AspectRatio   string
	Resolution    string
	// InputReferences are https URLs or data: URIs of reference images.
	InputReferences []string
	// Bind records which account serves the job and the upstream request id,
	// as soon as each is known.
	Bind func(accountID int64, provider, upstreamRequestID string)
	// Progress reports the upstream's completion percentage.
	Progress func(percent int)
}

// VideoBackendResult is a finished video.
type VideoBackendResult struct {
	URL      string
	Data     []byte
	MimeType string
}

// maxBackendVideoReferences bounds the reference images of a backend job.
const maxBackendVideoReferences = 8

// SetVideoBackend installs the backend for video routes the Web plane does not
// serve itself. It must be called before the handler serves requests.
func (h *Handler) SetVideoBackend(backend VideoBackend) {
	if h != nil {
		h.videoBackend = backend
	}
}

// backendVideoModel resolves a model against the installed backend.
func (h *Handler) backendVideoModel(ctx context.Context, modelID string) (string, bool) {
	if h == nil || h.videoBackend == nil {
		return "", false
	}
	return h.videoBackend.VideoModel(ctx, modelID)
}

// startBackendVideoJob validates and registers a job served by the backend.
func (h *Handler) startBackendVideoJob(w http.ResponseWriter, r *http.Request, req VideosRequest, upstreamModel string) {
	if err := h.ensureModelCapability(r.Context(), req.Model, store.CapabilityVideo); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	if len(req.InputReferences) > maxBackendVideoReferences {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("input_references supports at most %d images for this model", maxBackendVideoReferences))
		return
	}
	for _, reference := range req.InputReferences {
		if !validBackendVideoReference(reference) {
			httpserver.WriteError(w, http.StatusBadRequest, "input_references must be https URLs or data: image URIs")
			return
		}
	}
	cfg, err := validateVideoConfigForModel(&VideoConfig{
		VideoLength:    req.Seconds,
		ResolutionName: req.ResolutionName,
		Preset:         req.Preset,
		Size:           firstNonEmpty(req.Size, "16:9"),
	}, ModelSpec{ID: req.Model, UpstreamModel: upstreamModel, IsVideo: true}, len(req.InputReferences))
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	job := &videoJob{
		ID:              "video_" + randomHex(16),
		Model:           req.Model,
		Prompt:          req.Prompt,
		Seconds:         cfg.VideoLength,
		Size:            firstNonEmpty(cfg.Size, req.Size, cfg.AspectRatio),
		Quality:         "standard",
		CreatedAt:       time.Now().Unix(),
		Status:          "queued",
		InputReferences: req.InputReferences,
		Operation:       "generate",
		OwnerHash:       videoRequestOwner(r),
		PublicBaseURL:   detectPublicBaseURL(r),
	}
	putVideoJob(job)
	h.persistVideoJob(r.Context(), job)
	response := job.toMap()
	if cost, priced := pricing.EstimateVideoCost(upstreamModel, cfg.ResolutionName, cfg.VideoLength, len(req.InputReferences)); priced {
		h.settleMediaBilling(r.Context(), req.Model, cost, map[string]any{
			"plane": "backend", "endpoint": "videos", "job_id": job.ID, "seconds": cfg.VideoLength,
			"resolution": cfg.ResolutionName, "input_images": len(req.InputReferences),
		})
	}
	go h.runBackendVideoJob(context.Background(), job, upstreamModel, cfg)
	util.WriteJSON(w, response)
}

func validBackendVideoReference(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:image/")
}

func (h *Handler) runBackendVideoJob(ctx context.Context, job *videoJob, upstreamModel string, cfg *VideoConfig) {
	ctx, cancel := context.WithTimeout(ctx, videoJobTTL)
	defer cancel()
	release, _ := h.holdVideoLease(ctx, job)
	defer release()
	update := func(mutate func()) {
		videoJobsMu.Lock()
		mutate()
		videoJobsMu.Unlock()
		h.persistVideoJob(ctx, job)
	}
	update(func() { job.Status, job.Progress = "in_progress", 1 })
	result, err := h.videoBackend.RunVideo(ctx, VideoBackendJob{
		ID: job.ID, Model: job.Model, UpstreamModel: upstreamModel, Prompt: job.Prompt,
		Seconds: cfg.VideoLength, AspectRatio: cfg.AspectRatio, Resolution: cfg.ResolutionName,
		InputReferences: append([]string(nil), job.InputReferences...),
		Bind: func(accountID int64, provider, upstreamRequestID string) {
			update(func() {
				if accountID != 0 {
					job.AccountID = accountID
				}
				if provider != "" {
					job.Provider = provider
				}
				if upstreamRequestID != "" {
					job.UpstreamRequestID = upstreamRequestID
				}
			})
		},
		Progress: func(percent int) {
			update(func() { job.Progress = max(job.Progress, min(99, clampProgress(percent))) })
		},
	})
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	h.completeBackendVideoJob(ctx, job, result, update)
}
