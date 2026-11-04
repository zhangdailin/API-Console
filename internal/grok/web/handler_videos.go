package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
)

// videoJobTTL is how long a video job (and the one-shot upload ticket that
// belongs to it) stays addressable. grok2api keeps the same window.
const videoJobTTL = 2 * time.Hour

var (
	videoJobsMu sync.Mutex
	videoJobs   = map[string]*videoJob{}
)

func cleanupVideoJobsLocked(now time.Time) {
	for id, job := range videoJobs {
		if job == nil || now.Sub(time.Unix(job.CreatedAt, 0)) > videoJobTTL {
			delete(videoJobs, id)
		}
	}
}

func putVideoJob(job *videoJob) {
	videoJobsMu.Lock()
	defer videoJobsMu.Unlock()
	cleanupVideoJobsLocked(time.Now())
	videoJobs[job.ID] = job
}

func (j *videoJob) toMap() map[string]interface{} {
	if j.StandardAPI {
		return j.toStandardMap()
	}
	out := map[string]interface{}{
		"id":         j.ID,
		"object":     "video",
		"created_at": j.CreatedAt,
		"status":     j.Status,
		"model":      j.Model,
		"progress":   j.Progress,
		"prompt":     j.Prompt,
		"seconds":    fmt.Sprint(j.Seconds),
		"size":       j.Size,
		"quality":    j.Quality,
	}
	if j.CompletedAt > 0 {
		out["completed_at"] = j.CompletedAt
	}
	if strings.TrimSpace(j.VideoURL) != "" {
		out["video_url"] = j.VideoURL
	}
	if j.Status == "completed" && strings.TrimSpace(j.ID) != "" && strings.TrimSpace(j.ContentPath) != "" {
		out["content_url"] = "/grok/v1/videos/" + j.ID + "/content"
	}
	if j.Error != nil {
		out["error"] = j.Error
	}
	if strings.TrimSpace(j.RemixedFromID) != "" {
		out["remixed_from_video_id"] = j.RemixedFromID
	}
	return out
}

// videoContentURL builds the reachable content self-link for a video job.
func videoContentURL(id string) string {
	prefix := strings.TrimRight(strings.TrimSpace(grokMediaRoutePrefix), "/")
	if prefix == "" {
		prefix = "/v1"
	}
	return prefix + "/videos/" + strings.TrimSpace(id) + "/content"
}

// grokMediaRoutePrefix is the prefix the media routes are registered under.
// Both the grok-native and the unified prefix are served, so a link built here
// resolves on either; it is overridable for a deployment behind a path rewrite.
var grokMediaRoutePrefix = "/v1"

func (j *videoJob) toStandardMap() map[string]interface{} {
	if j == nil {
		return map[string]interface{}{"status": "failed", "error": map[string]interface{}{"code": "internal_error", "message": "video job is unavailable"}}
	}
	switch j.Status {
	case "completed":
		video := map[string]interface{}{
			// The self-link must resolve: it is emitted with the same prefix the
			// client called, so a /grok/v1 caller is not handed a /v1 path it
			// never configured.
			"url":                videoContentURL(j.ID),
			"respect_moderation": true,
		}
		if j.Operation == "generate" && j.Seconds > 0 {
			video["duration"] = j.Seconds
		}
		return map[string]interface{}{
			"status": "done", "model": j.Model, "progress": 100, "video": video,
		}
	case "failed":
		message := "video operation failed"
		code := "internal_error"
		if j.Error != nil {
			if value := strings.TrimSpace(fmt.Sprint(j.Error["message"])); value != "" && value != "<nil>" {
				message = value
			}
			if value := strings.TrimSpace(fmt.Sprint(j.Error["code"])); value != "" && value != "<nil>" {
				code = value
			}
		}
		// The stored code is kept: grok2api distinguishes an account or model
		// failure ("the account cannot serve this model") from an internal
		// fault, and collapsing both onto internal_error told a client to give
		// up on a job another account could have finished.
		return map[string]interface{}{
			"status": "failed", "error": map[string]interface{}{"code": code, "message": message},
		}
	default:
		return map[string]interface{}{
			"status": "pending", "model": j.Model, "progress": min(99, clampProgress(j.Progress)),
		}
	}
}

func videoRequestOwner(r *http.Request) string {
	if r == nil {
		return "anonymous"
	}
	owner := strings.TrimSpace(middleware.APIKeyFingerprint(r.Context()))
	if owner == "" {
		return "anonymous"
	}
	return owner
}

func storedVideoJobFromRuntime(job *videoJob) *store.StoredVideoJob {
	if job == nil {
		return nil
	}
	owner := strings.TrimSpace(job.OwnerHash)
	if owner == "" {
		owner = "anonymous"
	}
	errorCode := ""
	errorMessage := ""
	if job.Error != nil {
		errorCode = strings.TrimSpace(fmt.Sprint(job.Error["code"]))
		errorMessage = strings.TrimSpace(fmt.Sprint(job.Error["message"]))
		if errorCode == "<nil>" {
			errorCode = ""
		}
		if errorMessage == "<nil>" {
			errorMessage = ""
		}
	}
	return &store.StoredVideoJob{
		ID: job.ID, OwnerHash: owner, AccountID: job.AccountID, Provider: job.Provider,
		Model: job.Model, Prompt: truncateVideoJobText(job.Prompt, 64<<10),
		Seconds: job.Seconds, Size: job.Size, Quality: job.Quality,
		Status: job.Status, Progress: job.Progress, VideoURL: job.VideoURL,
		ContentPath: job.ContentPath, UpstreamRequestID: job.UpstreamRequestID, RemixedFromID: job.RemixedFromID,
		Operation: job.Operation, StandardAPI: job.StandardAPI, BuildFallback: job.BuildFallback,
		ErrorCode: truncateVideoJobText(errorCode, 256), ErrorMessage: truncateVideoJobText(errorMessage, 4<<10),
		CreatedAt: job.CreatedAt, CompletedAt: job.CompletedAt,
	}
}

func truncateVideoJobText(value string, limit int) string {
	if limit <= 0 || len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func cloneVideoJob(job *videoJob) *videoJob {
	if job == nil {
		return nil
	}
	cloned := *job
	cloned.InputReferences = append([]string(nil), job.InputReferences...)
	if job.Error != nil {
		cloned.Error = make(map[string]interface{}, len(job.Error))
		for key, value := range job.Error {
			cloned.Error[key] = value
		}
	}
	return &cloned
}

func runtimeVideoJobFromStored(stored *store.StoredVideoJob) *videoJob {
	if stored == nil {
		return nil
	}
	job := &videoJob{
		ID: stored.ID, OwnerHash: stored.OwnerHash, AccountID: stored.AccountID, Provider: stored.Provider,
		Model: stored.Model, Prompt: stored.Prompt,
		Seconds: stored.Seconds, Size: stored.Size, Quality: stored.Quality,
		Status: stored.Status, Progress: stored.Progress, VideoURL: stored.VideoURL,
		ContentPath: stored.ContentPath, UpstreamRequestID: stored.UpstreamRequestID, RemixedFromID: stored.RemixedFromID,
		Operation: stored.Operation, StandardAPI: stored.StandardAPI, BuildFallback: stored.BuildFallback,
		CreatedAt: stored.CreatedAt, CompletedAt: stored.CompletedAt,
	}
	if stored.ErrorCode != "" || stored.ErrorMessage != "" {
		job.Error = map[string]interface{}{"code": stored.ErrorCode, "message": stored.ErrorMessage}
	}
	if !validPersistedVideoContentPath(job.ContentPath) {
		job.ContentPath = ""
	}
	return job
}

func validPersistedVideoContentPath(path string) bool {
	return strings.TrimSpace(path) == "" || validCachedMediaContentPath(path, "video")
}

func (h *Handler) persistVideoJob(ctx context.Context, job *videoJob) {
	if h == nil || h.lb == nil || h.lb.Store == nil || job == nil {
		return
	}
	videoJobsMu.Lock()
	snapshot := storedVideoJobFromRuntime(job)
	videoJobsMu.Unlock()
	if snapshot == nil {
		return
	}
	createdAt := time.Unix(snapshot.CreatedAt, 0)
	if snapshot.CreatedAt <= 0 {
		createdAt = time.Now()
	}
	ttl := time.Until(createdAt.Add(videoJobTTL))
	if ttl <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := h.lb.Store.SaveStoredVideoJob(persistCtx, snapshot, ttl); err != nil {
		slog.Warn("failed to persist video job", "video_id", snapshot.ID, "status", snapshot.Status, "error", err)
	}
}

func (h *Handler) lookupVideoJob(ctx context.Context, id, owner string) (*videoJob, bool) {
	videoJobsMu.Lock()
	cleanupVideoJobsLocked(time.Now())
	if job, ok := videoJobs[strings.TrimSpace(id)]; ok {
		jobOwner := strings.TrimSpace(job.OwnerHash)
		if jobOwner == "" {
			jobOwner = "anonymous"
		}
		if jobOwner == owner {
			snapshot := cloneVideoJob(job)
			videoJobsMu.Unlock()
			return snapshot, true
		}
	}
	videoJobsMu.Unlock()
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil, false
	}
	stored, err := h.lb.Store.GetStoredVideoJob(ctx, id, owner)
	if err != nil {
		return nil, false
	}
	job := runtimeVideoJobFromStored(stored)
	if job == nil {
		return nil, false
	}
	putVideoJob(job)
	return cloneVideoJob(job), true
}

func (h *Handler) HandleVideosCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	req, err := parseVideosRequest(r)
	if err != nil {
		writeGrokError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if req.Seconds == 0 {
		req.Seconds = 8
	}
	req.Model = normalizeModelID(firstNonEmpty(req.Model, "grok-imagine-video"))
	if !requireAPIKeyModel(w, r, req.Model) {
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeGrokError(w, http.StatusBadRequest, "prompt cannot be empty")
		return
	}
	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsVideo {
		writeGrokError(w, http.StatusBadRequest, fmt.Sprintf("Model %q is not a video model", req.Model))
		return
	}
	spec = h.applyPersistedRoute(r.Context(), spec)
	if err := h.ensureModelCapability(r.Context(), req.Model, store.CapabilityVideo); err != nil {
		writeGrokError(w, http.StatusBadRequest, modelValidationMessage(req.Model, err))
		return
	}
	if len(req.InputReferences) > 0 {
		writeGrokError(w, http.StatusBadRequest, "web video generation currently supports text-to-video only")
		return
	}
	h.startVideoJob(w, r, req, spec)
}

// startVideoJob validates the video configuration, registers the job and starts
// its worker. It is shared by the legacy /videos endpoint and by the standard
// /videos/generations endpoint when the resolved model is routed to the Web
// plane rather than to Console.
func (h *Handler) startVideoJob(w http.ResponseWriter, r *http.Request, req VideosRequest, spec ModelSpec) {
	cfg, err := validateVideoConfigForModel(&VideoConfig{
		VideoLength:    req.Seconds,
		ResolutionName: req.ResolutionName,
		Preset:         req.Preset,
		Size:           firstNonEmpty(req.Size, "16:9"),
	}, spec, len(req.InputReferences))
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	maxReferences := 7
	if len(req.InputReferences) > maxReferences {
		writeGrokError(w, http.StatusBadRequest, fmt.Sprintf("input_references supports at most %d images for this model", maxReferences))
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
		Progress:        0,
		InputReferences: req.InputReferences,
		Operation:       "generate",
		OwnerHash:       videoRequestOwner(r),
		PublicBaseURL:   detectPublicBaseURL(r),
	}
	putVideoJob(job)
	h.persistVideoJob(r.Context(), job)
	response := job.toMap()
	// Video is billed from the request itself (duration, resolution and reference
	// images), which is what grok2api prices at creation.
	if cost, priced := pricing.EstimateVideoCost(spec.UpstreamModel, firstNonEmpty(cfg.ResolutionName, req.ResolutionName), cfg.VideoLength, len(req.InputReferences)); priced {
		h.settleMediaBilling(r.Context(), req.Model, cost, map[string]interface{}{
			"plane": "web", "endpoint": "videos", "job_id": job.ID, "seconds": cfg.VideoLength,
			"resolution": firstNonEmpty(cfg.ResolutionName, req.ResolutionName), "input_images": len(req.InputReferences),
		})
	}
	go h.runVideoCreateJob(context.Background(), job, spec, cfg)
	writeJSON(w, response)
}

// webFreeVideoDurationLimit is the longest video a free/basic Web credential may
// request on the app-chat plane.
const webFreeVideoDurationLimit = 6

// webFreeVideoDurationCap returns the cap that applies to this credential, or 0
// when the account is not a free Web account.
func webFreeVideoDurationCap(acc *store.Account) int {
	if acc == nil {
		return 0
	}
	if ProviderForAccount(acc) != ProviderWeb {
		return 0
	}
	switch strings.ToLower(strings.TrimSpace(acc.Subscription)) {
	case "free", "basic", "lite":
		return webFreeVideoDurationLimit
	default:
		return 0
	}
}

func (h *Handler) runVideoCreateJob(ctx context.Context, job *videoJob, spec ModelSpec, cfg *VideoConfig) {
	update := func(status string, progress int) {
		videoJobsMu.Lock()
		job.Status = status
		if progress >= 0 {
			job.Progress = clampProgress(progress)
		}
		videoJobsMu.Unlock()
		h.persistVideoJob(ctx, job)
	}
	update("in_progress", 1)

	// Retry opening a session with account switching — video generation
	// can take minutes and the 429 cooldown may clear while we wait.
	var sess *chatAccountSession
	var sessErr error
	resolution := ""
	if cfg != nil {
		resolution = cfg.ResolutionName
	}
	ctx = withMediaProduct(ctx, mediaProductFor(spec, resolution))
	for attempt := 0; attempt < 10; attempt++ {
		sess, sessErr = h.openChatAccountSessionForModel(ctx, spec)
		if sessErr == nil {
			break
		}
		var media *mediaUnavailableError
		if errors.As(sessErr, &media) {
			break // waiting does not restore a spent media allowance
		}
		time.Sleep(2 * time.Second)
	}
	if sessErr != nil {
		h.failVideoJob(job, sessErr)
		return
	}
	defer sess.Close()

	// A free Web credential cannot fetch a duration beyond the included tier
	// cap: the upstream rejects the whole job, which used to surface as an
	// opaque failure minutes later. Clamping here matches what grok2api does
	// before it ever sends the request.
	if cap := webFreeVideoDurationCap(sess.acc); cap > 0 && cfg.VideoLength > cap {
		cfg.VideoLength = cap
	}

	attachments := make([]AttachmentInput, 0, len(job.InputReferences))
	for _, ref := range job.InputReferences {
		attachments = append(attachments, AttachmentInput{Type: "image", Data: ref})
	}
	artifact, err := h.runVideoSegments(ctx, sess, spec, job.Prompt, attachments, cfg, nil, func(progress int) {
		update("in_progress", progress)
	})
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	raw, _, err := h.webClient().downloadAsset(ctx, sess.token, artifact.URL)
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	name, err := h.cacheMediaBytes(artifact.URL, "video", raw, "video/mp4")
	if err != nil {
		h.failVideoJob(job, err)
		return
	}
	videoJobsMu.Lock()
	job.Status = "completed"
	job.Progress = 100
	job.CompletedAt = time.Now().Unix()
	job.VideoURL = artifact.URL
	job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
	job.RemixedFromID = artifact.VideoPostID
	videoJobsMu.Unlock()
	h.persistVideoJob(ctx, job)
}

func (h *Handler) failVideoJob(job *videoJob, err error) {
	h.failVideoJobWithCode(job, "video_generation_failed", err)
}

func (h *Handler) failVideoJobWithCode(job *videoJob, code string, err error) {
	// An asynchronous job is read back with a 200, so its stored message is a
	// client-facing body: a pool failure is classified (a capacity problem reads as
	// retryable and never carries the pool's own note), and the note stays in the
	// log with the rest of the diagnostics.
	message, fromPool := videoJobFailureMessage(err)
	if fromPool {
		slog.Warn("Grok account pool could not serve a video job",
			"error", err, "job_id", job.ID)
	} else {
		slog.Error("Video job failed", "error", err, "job_id", job.ID, "code", code)
	}
	videoJobsMu.Lock()
	job.Status = "failed"
	job.Error = map[string]interface{}{
		"code":    code,
		"message": message,
	}
	videoJobsMu.Unlock()
	h.persistVideoJob(context.Background(), job)
}

// videoJobFailureMessage returns the message an asynchronous job stores for the
// client, and whether it is the classified answer for a pool failure.
//
// A job is retrieved with a 200, so what is stored is a client-facing body: the
// pool's own note ("no enabled accounts available for channel: grok …") must not
// be what a polling client reads, and neither must the upstream's prose — both go
// to the log. The job's error_code carries the machine-readable kind, and a local
// failure keeps its own precise message.
func videoJobFailureMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if out := apperrors.ClassifyPoolExhaustion(err, ""); !out.Empty() {
		return out.Message, true
	}
	return grokUpstreamFailureMessage(err), false
}
