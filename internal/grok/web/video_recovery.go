package web

import (
	"context"
	"errors"
	"log/slog"
	"orchids-api/internal/util"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/loadbalancer"
	"orchids-api/internal/store"
)

// A video job runs in one process, but its record lives in Redis and is read
// by every instance. A job holds a lease while it runs; a job whose lease has
// lapsed lost its process (a restart, a crash). The recovery sweep takes such
// a job over: a backend job that already reached its upstream is polled again
// on the same account, and anything else is failed, so a client polling it is
// not left waiting out the job's TTL.

const (
	videoLeaseTTL       = 45 * time.Second
	videoLeaseRefresh   = 15 * time.Second
	videoRecoveryPeriod = time.Minute
	// videoRecoveryGrace skips jobs younger than this: their creator takes the
	// lease right after it registers the job.
	videoRecoveryGrace = 2 * videoLeaseTTL
)

// VideoResumer is the optional part of a VideoBackend that continues a job
// whose upstream request was already created.
type VideoResumer interface {
	ResumeVideo(ctx context.Context, job VideoBackendJob, accountID int64, upstreamRequestID string) (VideoBackendResult, error)
}

var (
	runningVideoMu sync.Mutex
	runningVideo   = map[string]bool{}
)

func markVideoRunning(id string, running bool) {
	runningVideoMu.Lock()
	defer runningVideoMu.Unlock()
	if running {
		runningVideo[id] = true
	} else {
		delete(runningVideo, id)
	}
}

func videoRunningHere(id string) bool {
	runningVideoMu.Lock()
	defer runningVideoMu.Unlock()
	return runningVideo[id]
}

func videoLeaseOwner(job *videoJob) string {
	return firstNonEmpty(strings.TrimSpace(job.OwnerHash), "anonymous")
}

type videoLeaseContextKey struct{}

// holdVideoLease gives each worker a distinct fence and cancels work on lease loss.
func (h *Handler) holdVideoLease(ctx context.Context, job *videoJob) (context.Context, func(), bool) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		markVideoRunning(job.ID, true)
		return ctx, func() { markVideoRunning(job.ID, false) }, true
	}
	st := h.lb.Store
	owner := videoLeaseOwner(job)
	holder := h.instanceID + ":" + util.RandomHex(16)
	acquired, err := st.AcquireVideoJobLease(ctx, job.ID, owner, holder, videoLeaseTTL)
	if err != nil {
		slog.Warn("video job lease unavailable", "video_id", job.ID, "error", err)
	}
	if err != nil || !acquired {
		return ctx, func() {}, false
	}
	markVideoRunning(job.ID, true)
	worker, cancelWorker := context.WithCancel(context.WithValue(ctx, videoLeaseContextKey{}, holder))
	go func() {
		ticker := time.NewTicker(videoLeaseRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-worker.Done():
				return
			case <-ticker.C:
				refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				held, err := st.RefreshVideoJobLease(refreshCtx, job.ID, owner, holder, videoLeaseTTL)
				cancel()
				if err != nil || !held {
					slog.Warn("video job lease lost", "video_id", job.ID, "error", err)
					cancelWorker()
					return
				}
			}
		}
	}()
	var once sync.Once
	return worker, func() {
		once.Do(func() {
			cancelWorker()
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = st.ReleaseVideoJobLease(releaseCtx, job.ID, owner, holder)
			cancel()
			markVideoRunning(job.ID, false)
		})
	}, true
}

// StartVideoJobRecovery runs the recovery sweep until ctx ends.
func (h *Handler) StartVideoJobRecovery(ctx context.Context) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(videoRecoveryPeriod)
		defer ticker.Stop()
		for {
			h.RecoverVideoJobs(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// RecoverVideoJobs runs one recovery sweep: it takes over every unfinished
// job whose lease has lapsed. Resumed and failed jobs finish in the background.
func (h *Handler) RecoverVideoJobs(ctx context.Context) {
	if h == nil || h.lb == nil || h.lb.Store == nil {
		return
	}
	now := time.Now()
	stored, err := h.lb.Store.ListStoredVideoJobs(ctx)
	if err != nil {
		if !strings.Contains(err.Error(), "not configured") {
			slog.Warn("video job recovery could not list jobs", "error", err)
		}
		return
	}
	for _, record := range stored {
		if record == nil || store.TerminalVideoJobStatus(record.Status) || videoRunningHere(record.ID) {
			continue
		}
		if record.CreatedAt > 0 && now.Sub(time.Unix(record.CreatedAt, 0)) < videoRecoveryGrace {
			continue
		}
		h.takeOverVideoJob(ctx, record)
	}
}

func (h *Handler) takeOverVideoJob(ctx context.Context, record *store.StoredVideoJob) {
	stale := runtimeVideoJobFromStored(record)
	worker, release, ok := h.holdVideoLease(ctx, stale)
	if !ok {
		return
	}
	current, err := h.lb.Store.GetStoredVideoJob(worker, stale.ID, videoLeaseOwner(stale))
	if err != nil || store.TerminalVideoJobStatus(current.Status) {
		release()
		return
	}
	job := runtimeVideoJobFromStored(current)
	putVideoJob(job)
	go h.recoverVideoJob(worker, job, release)
}

func (h *Handler) recoverVideoJob(ctx context.Context, job *videoJob, release func()) {
	ctx, closeLeaseScope := loadbalancer.WithConnLeaseScope(ctx)
	defer closeLeaseScope()
	defer release()
	resumer, canResume := h.videoBackend.(VideoResumer)
	upstream := ""
	if canResume && job.AccountID != 0 && strings.TrimSpace(job.UpstreamRequestID) != "" {
		upstream, canResume = h.backendVideoModel(ctx, job.Model)
	} else {
		canResume = false
	}
	if !canResume {
		slog.Info("video job interrupted by a restart; failing it", "video_id", job.ID, "provider", job.Provider)
		h.failVideoJobWithCode(ctx, job, "video_interrupted", errors.New("the video job was interrupted by a gateway restart; submit it again"))
		return
	}
	slog.Info("resuming an interrupted video job", "video_id", job.ID, "provider", job.Provider, "account_id", job.AccountID)
	ctx, cancel := context.WithTimeout(ctx, videoJobTTL)
	defer cancel()
	update := func(mutate func()) {
		videoJobsMu.Lock()
		mutate()
		videoJobsMu.Unlock()
		h.persistVideoJob(ctx, job)
	}
	result, err := resumer.ResumeVideo(ctx, VideoBackendJob{
		ID: job.ID, Model: job.Model, UpstreamModel: upstream, Prompt: job.Prompt, Seconds: job.Seconds,
		Bind: func(int64, string, string) {},
		Progress: func(percent int) {
			update(func() { job.Progress = max(job.Progress, min(99, clampProgress(percent))) })
		},
	}, job.AccountID, job.UpstreamRequestID)
	if err != nil {
		h.failVideoJob(ctx, job, err)
		return
	}
	h.completeBackendVideoJob(ctx, job, result, update)
}

// completeBackendVideoJob caches a finished backend video and marks the job.
func (h *Handler) completeBackendVideoJob(ctx context.Context, job *videoJob, result VideoBackendResult, update func(func())) {
	if result.Body == nil {
		h.failVideoJob(ctx, job, errors.New("video backend returned no content"))
		return
	}
	name, err := cacheVideoStream(firstNonEmpty(result.URL, job.ID), result.Body, firstNonEmpty(result.MimeType, "video/mp4"))
	if err != nil {
		h.failVideoJob(ctx, job, err)
		return
	}
	update(func() {
		job.Status, job.Progress, job.CompletedAt = "completed", 100, time.Now().Unix()
		job.VideoURL = result.URL
		job.ContentPath = filepath.Join(cacheBaseDir, "video", name)
	})
}
