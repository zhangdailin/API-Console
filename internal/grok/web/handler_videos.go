package web

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"orchids-api/internal/grok/grokbase"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	apperrors "orchids-api/internal/errors"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/middleware"
	"orchids-api/internal/pricing"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
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

func (j *videoJob) toMap() map[string]any {
	if j.StandardAPI {
		return j.toStandardMap()
	}
	out := map[string]any{
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
	if j.Status == "completed" && strings.TrimSpace(j.ID) != "" && (j.ContentPath != "" || videoOrigin(j) != "") {
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

func (j *videoJob) toStandardMap() map[string]any {
	if j == nil {
		return map[string]any{"status": "failed", "error": map[string]any{"code": "internal_error", "message": "video job is unavailable"}}
	}
	switch j.Status {
	case "completed":
		video := map[string]any{
			// The self-link must resolve: it is emitted with the same prefix the
			// client called, so a /grok/v1 caller is not handed a /v1 path it
			// never configured.
			"url":                videoContentURL(j.ID),
			"respect_moderation": true,
		}
		if j.Operation == "generate" && j.Seconds > 0 {
			video["duration"] = j.Seconds
		}
		return map[string]any{
			"status": "done", "model": j.Model, "progress": 100, "video": video,
		}
	case "failed":
		message := "video operation failed"
		code := "internal_error"
		if j.Error != nil {
			if value := videoJobErrorField(j.Error, "message"); value != "" {
				message = value
			}
			if value := videoJobErrorField(j.Error, "code"); value != "" {
				code = value
			}
		}
		// The stored code is kept: grok2api distinguishes an account or model
		// failure ("the account cannot serve this model") from an internal
		// fault, and collapsing both onto internal_error told a client to give
		// up on a job another account could have finished.
		return map[string]any{
			"status": "failed", "error": map[string]any{"code": code, "message": message},
		}
	default:
		return map[string]any{
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
	stored := job.StoredVideoJob
	stored.OwnerHash = firstNonEmpty(stored.OwnerHash, "anonymous")
	stored.Prompt = truncateVideoJobText(stored.Prompt, 64<<10)
	stored.ContentName = videoContentName(stored.ContentPath)
	stored.ErrorCode = truncateVideoJobText(videoJobErrorField(job.Error, "code"), 256)
	stored.ErrorMessage = truncateVideoJobText(videoJobErrorField(job.Error, "message"), 4<<10)
	return &stored
}

func videoJobErrorField(fields map[string]any, key string) string {
	value := strings.TrimSpace(fmt.Sprint(fields[key]))
	if value == "<nil>" {
		return ""
	}
	return value
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
	if job.Error != nil {
		cloned.Error = make(map[string]any, len(job.Error))
		maps.Copy(cloned.Error, job.Error)
	}
	return &cloned
}

func runtimeVideoJobFromStored(stored *store.StoredVideoJob) *videoJob {
	if stored == nil {
		return nil
	}
	job := &videoJob{StoredVideoJob: *stored}
	if stored.ErrorCode != "" || stored.ErrorMessage != "" {
		job.Error = map[string]any{"code": stored.ErrorCode, "message": stored.ErrorMessage}
	}
	if name := videoContentName(stored.ContentName); name != "" && name == stored.ContentName {
		job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
	} else if !validPersistedVideoContentPath(job.ContentPath) {
		job.ContentPath = ""
	}
	return job
}

func validPersistedVideoContentPath(path string) bool {
	return strings.TrimSpace(path) == "" || validCachedMediaContentPath(path, "video")
}

func (h *Handler) persistVideoJob(ctx context.Context, job *videoJob) error {
	if h == nil || h.lb == nil || h.lb.Store == nil || job == nil {
		return nil
	}
	videoJobsMu.Lock()
	snapshot := storedVideoJobFromRuntime(job)
	videoJobsMu.Unlock()
	if snapshot == nil {
		return nil
	}
	createdAt := time.Unix(snapshot.CreatedAt, 0)
	if snapshot.CreatedAt <= 0 {
		createdAt = time.Now()
	}
	ttl := time.Until(createdAt.Add(videoJobTTL))
	if ttl <= 0 {
		return store.ErrVideoJobLeaseLost
	}
	if ctx == nil {
		ctx = context.Background()
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	var err error
	if holder, ok := ctx.Value(videoLeaseContextKey{}).(string); ok {
		err = h.lb.Store.SaveLeasedVideoJob(persistCtx, snapshot, ttl, holder)
	} else {
		err = h.lb.Store.SaveStoredVideoJob(persistCtx, snapshot, ttl)
	}
	if err != nil {
		slog.Warn("failed to persist video job", "video_id", snapshot.ID, "status", snapshot.Status, "error", err)
	}
	return err
}

func (h *Handler) lookupVideoJob(ctx context.Context, id, owner string) (*videoJob, error) {
	videoJobsMu.Lock()
	cleanupVideoJobsLocked(time.Now())
	if job, ok := videoJobs[strings.TrimSpace(id)]; ok {
		jobOwner := strings.TrimSpace(job.OwnerHash)
		if jobOwner == "" {
			jobOwner = "anonymous"
		}
		if jobOwner == owner && (videoRunningHere(job.ID) || h == nil || h.lb == nil || h.lb.Store == nil) {
			snapshot := cloneVideoJob(job)
			videoJobsMu.Unlock()
			return snapshot, nil
		}
	}
	videoJobsMu.Unlock()
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return nil, store.ErrNoRows
	}
	stored, err := h.lb.Store.GetStoredVideoJob(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	job := runtimeVideoJobFromStored(stored)
	if job == nil {
		return nil, store.ErrNoRows
	}
	return job, nil
}

func (h *Handler) HandleVideosCreate(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
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
		// Name an unknown field: "invalid request" left a stray client field
		// (a saved reasoning_effort) undiagnosable from the page.
		message := "invalid request"
		if unknown, ok := errors.AsType[*unknownVideoFieldError](err); ok {
			message = unknown.Error()
		}
		httpserver.WriteError(w, http.StatusBadRequest, message)
		return
	}
	if req.Seconds == 0 {
		req.Seconds = 8
	}
	req.Model = normalizeModelID(firstNonEmpty(req.Model, "grok-imagine-video"))
	if !httpserver.RequireAPIKeyModel(w, r, req.Model) {
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		httpserver.WriteError(w, http.StatusBadRequest, "prompt cannot be empty")
		return
	}
	if upstreamModel, ok := h.backendVideoModel(r.Context(), req.Model); ok {
		h.startBackendVideoJob(w, r, req, upstreamModel)
		return
	}
	spec, ok := ResolveModel(req.Model)
	if !ok || !spec.IsVideo {
		httpserver.WriteError(w, http.StatusBadRequest, fmt.Sprintf("Model %q is not a video model", req.Model))
		return
	}
	spec = h.applyPersistedRoute(r.Context(), spec)
	if err := h.ensureModelCapability(r.Context(), req.Model, store.CapabilityVideo); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, grokbase.ModelValidationMessage(req.Model, err))
		return
	}
	if len(req.InputReferences) > 0 {
		httpserver.WriteError(w, http.StatusBadRequest, "web video generation currently supports text-to-video only")
		return
	}
	h.startVideoJob(w, r, req, spec)
}

// videoConfig applies the shared request defaults before model-specific validation.
func (req VideosRequest) videoConfig(spec ModelSpec) (*VideoConfig, error) {
	return validateVideoConfigForModel(&VideoConfig{
		VideoLength:    req.Seconds,
		ResolutionName: req.ResolutionName,
		Preset:         req.Preset,
		Size:           firstNonEmpty(req.Size, "16:9"),
	}, spec, len(req.InputReferences))
}

func (h *Handler) startVideoJob(w http.ResponseWriter, r *http.Request, req VideosRequest, spec ModelSpec) {
	cfg, err := req.videoConfig(spec)
	if err != nil {
		writeGrokUpstreamError(w, err)
		return
	}
	h.enqueueVideo(w, r, req, cfg, spec.UpstreamModel, "web", firstNonEmpty(cfg.ResolutionName, req.ResolutionName),
		func(ctx context.Context, job *videoJob) { h.runVideoCreateJob(ctx, job, spec, cfg) })
}

// enqueueVideo persists ownership before starting a worker and bills the
// accepted request. Each plane supplies its own validation and worker.
func (h *Handler) enqueueVideo(w http.ResponseWriter, r *http.Request, req VideosRequest, cfg *VideoConfig,
	upstreamModel, plane, resolution string, run func(context.Context, *videoJob),
) {
	job := &videoJob{
		StoredVideoJob: store.StoredVideoJob{
			ID:        "video_" + util.RandomHex(16),
			Model:     req.Model,
			Prompt:    req.Prompt,
			Seconds:   cfg.VideoLength,
			Size:      firstNonEmpty(cfg.Size, req.Size, cfg.AspectRatio),
			Quality:   "standard",
			CreatedAt: time.Now().Unix(),
			Status:    "queued",
			Operation: "generate",
			OwnerHash: videoRequestOwner(r),
		},
		PublicBaseURL: detectPublicBaseURL(r),
	}
	ctx := r.Context()
	if h.persistVideoJob(ctx, job) != nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "Could not register video job")
		return
	}
	putVideoJob(job)
	response := job.toMap()
	if cost, priced := pricing.EstimateVideoCost(upstreamModel, resolution, cfg.VideoLength, len(req.InputReferences)); priced {
		h.settleMediaBilling(ctx, req.Model, cost, map[string]any{
			"plane": plane, "endpoint": "videos", "job_id": job.ID, "seconds": cfg.VideoLength,
			"resolution": resolution, "input_images": len(req.InputReferences),
		})
	}
	go run(context.Background(), job)
	util.WriteJSON(w, response)
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
	if !acc.IsGrokWeb() {
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
	ctx, closeLeaseScope := loadbalancer.WithConnLeaseScope(ctx)
	defer closeLeaseScope()
	ctx, release, ok := h.holdVideoLease(ctx, job)
	if !ok {
		return
	}
	defer release()
	update := func(status string, progress int) error {
		videoJobsMu.Lock()
		job.Status = status
		if progress >= 0 {
			job.Progress = clampProgress(progress)
		}
		videoJobsMu.Unlock()
		return h.persistVideoJob(ctx, job)
	}
	if update("in_progress", 1) != nil {
		return
	}

	// Retry opening a session with account switching — video generation
	// can take minutes and the 429 cooldown may clear while we wait.
	var sess *chatAccountSession
	var sessErr error
	resolution := ""
	if cfg != nil {
		resolution = cfg.ResolutionName
	}
	ctx = withMediaProduct(ctx, mediaProductFor(spec, resolution))
	for range 10 {
		sess, sessErr = h.openChatAccountSessionForModel(ctx, spec)
		if sessErr == nil {
			break
		}
		if _, ok := errors.AsType[*mediaUnavailableError](sessErr); ok {
			break // waiting does not restore a spent media allowance
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	if sessErr != nil {
		h.failVideoJob(ctx, job, sessErr)
		return
	}
	defer sess.Close()
	videoJobsMu.Lock()
	job.AccountID = sess.acc.ID
	job.Provider = "web"
	videoJobsMu.Unlock()
	if h.persistVideoJob(ctx, job) != nil {
		return
	}

	// A free Web credential cannot fetch a duration beyond the included tier
	// cap: the upstream rejects the whole job, which used to surface as an
	// opaque failure minutes later. Clamping here matches what grok2api does
	// before it ever sends the request.
	if cap := webFreeVideoDurationCap(sess.acc); cap > 0 && cfg.VideoLength > cap {
		cfg.VideoLength = cap
	}

	artifact, err := h.runVideoGeneration(ctx, sess, spec, job.Prompt, nil, cfg, nil, func(progress int) {
		update("in_progress", progress)
	})
	if err != nil {
		h.failVideoJob(ctx, job, err)
		return
	}
	body, _, err := h.webClient().openAsset(ctx, sess.token, artifact.URL)
	if err != nil {
		h.failVideoJob(ctx, job, err)
		return
	}
	name, err := cacheVideoStream(artifact.URL, body, "video/mp4")
	if err != nil {
		h.failVideoJob(ctx, job, err)
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

func (h *Handler) failVideoJob(ctx context.Context, job *videoJob, err error) {
	h.failVideoJobWithCode(ctx, job, "video_generation_failed", err)
}

func (h *Handler) failVideoJobWithCode(ctx context.Context, job *videoJob, code string, err error) {
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
	job.Error = map[string]any{
		"code":    code,
		"message": message,
	}
	videoJobsMu.Unlock()
	h.persistVideoJob(ctx, job)
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
