package web

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"strings"

	"orchids-api/internal/httpserver"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// The video job runtime (ids, ownership, persistence, the content endpoint)
// lives in this package. A video route of another plane of the grok channel
// plugs its upstream in through VideoBackend and reuses all of it.

// VideoBackend serves the video routes of another plane.
type VideoBackend interface {
	// VideoModel reports whether modelID is one of the backend's observed
	// video routes, and names the upstream model it maps to.
	VideoModel(ctx context.Context, modelID string) (upstreamModel string, ok bool)
	// RunVideo creates the video upstream, waits for it and returns its content stream.
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

// VideoBackendResult transfers ownership of the finished video body to the caller.
type VideoBackendResult struct {
	URL      string
	Body     io.ReadCloser
	MimeType string
}

// maxBackendVideoReferences bounds the reference images of a backend job.
const maxBackendVideoReferences = 8

// MaxVideoInlineReferenceBytes bounds encoded inline reference images for a job.
const MaxVideoInlineReferenceBytes = 32 << 20

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
		httpserver.WriteError(w, http.StatusBadRequest, grokbase.ModelValidationMessage(req.Model, err))
		return
	}
	if len(req.InputReferences) > maxBackendVideoReferences {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("input_references supports at most %d images for this model", maxBackendVideoReferences))
		return
	}
	inline := 0
	for _, reference := range req.InputReferences {
		if !validBackendVideoReference(reference) {
			httpserver.WriteError(w, http.StatusBadRequest, "input_references must be https URLs or data: image URIs")
			return
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(reference)), "data:") {
			inline += len(strings.TrimSpace(reference))
			if inline > MaxVideoInlineReferenceBytes {
				httpserver.WriteError(w, http.StatusBadRequest, "combined inline reference images exceed 32 MiB")
				return
			}
		}
	}
	cfg, err := req.videoConfig(ModelSpec{ID: req.Model, UpstreamModel: upstreamModel, IsVideo: true})
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	h.enqueueVideo(w, r, req, cfg, upstreamModel, "backend", cfg.ResolutionName,
		func(ctx context.Context, job *videoJob) {
			h.runBackendVideoJob(ctx, job, upstreamModel, cfg, req.InputReferences)
		})
}

func validBackendVideoReference(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	return strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:image/")
}

func (h *Handler) runBackendVideoJob(ctx context.Context, job *videoJob, upstreamModel string, cfg *VideoConfig, references []string) {
	ctx, closeLeaseScope := loadbalancer.WithConnLeaseScope(ctx)
	defer closeLeaseScope()
	ctx, cancel := context.WithTimeout(ctx, videoJobTTL)
	defer cancel()
	ctx, release, ok := h.holdVideoLease(ctx, job)
	if !ok {
		return
	}
	defer release()
	videoJobsMu.Lock()
	job.Status, job.Progress = "in_progress", 1
	videoJobsMu.Unlock()
	if h.persistVideoJob(ctx, job) != nil {
		return
	}
	update := func(mutate func()) {
		videoJobsMu.Lock()
		mutate()
		videoJobsMu.Unlock()
		h.persistVideoJob(ctx, job)
	}
	result, err := h.videoBackend.RunVideo(ctx, VideoBackendJob{
		ID: job.ID, Model: job.Model, UpstreamModel: upstreamModel, Prompt: job.Prompt,
		Seconds: cfg.VideoLength, AspectRatio: cfg.AspectRatio, Resolution: cfg.ResolutionName,
		InputReferences: references,
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
		h.failVideoJob(ctx, job, err)
		return
	}
	h.completeBackendVideoJob(ctx, job, result, update)
}
