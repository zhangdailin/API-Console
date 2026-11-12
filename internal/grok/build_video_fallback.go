package grok

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"orchids-api/internal/accountpolicy"
	"orchids-api/internal/config"
	"orchids-api/internal/grok/grokbase"
	"orchids-api/internal/grok/web"
	"orchids-api/internal/store"
)

// Build video fallback. A paid Build account the CLI proxy refuses (403) for
// a video may still create it on the xAI API with the same OAuth token, but
// that route returns no video: xAI uploads the finished file to an upload_url
// the caller supplies. The gateway issues a one-time URL under its public
// origin, receives the PUT and hands the bytes to the job.
//
// The fallback is off unless grok_build_video_upload_base_url is set, because
// it needs a public route reachable from xAI. The upload token is the only
// authorization of that route: it is 32 random bytes, stored only as a hash,
// valid once and for two hours.

const (
	defaultBuildFallbackBaseURL = "https://api.x.ai/v1"
	buildFallbackVideoModel     = "grok-imagine-video-1.5-preview"
	videoUploadTicketTTL        = 2 * time.Hour
	videoUploadMaxBytes         = 512 << 20
	videoUploadWait             = 2 * time.Minute
	videoUploadReadDeadline     = 15 * time.Minute
	// VideoUploadPath is where xAI PUTs a finished fallback video.
	VideoUploadPath = "/media/uploads/"
)

var errVideoUploadNotReceived = errors.New("xAI finished the video but its upload has not arrived")

// videoUploadTicket is one issued upload URL. Path is where the bytes land.
type videoUploadTicket struct {
	Path      string    `json:"path"`
	ExpiresAt time.Time `json:"expires_at"`
	Received  bool      `json:"received"`
	MimeType  string    `json:"mime_type,omitempty"`
}

// videoUploads keeps tickets in Redis so any instance behind the public
// origin can receive the PUT, with an in-memory fallback for a store-less
// handler. The bytes are written under the media directory, which must be
// shared between instances for a multi-instance deployment.
type videoUploads struct {
	redis  *redis.Client
	prefix string
	mu     sync.Mutex
	local  map[string]videoUploadTicket
}

func (u *videoUploads) key(hash string) string { return u.prefix + "grok:video_upload:" + hash }

func (u *videoUploads) save(ctx context.Context, hash string, ticket videoUploadTicket) error {
	if u.redis != nil {
		raw, _ := json.Marshal(ticket)
		return u.redis.Set(ctx, u.key(hash), raw, time.Until(ticket.ExpiresAt)).Err()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.local == nil {
		u.local = map[string]videoUploadTicket{}
	}
	u.local[hash] = ticket
	return nil
}

func (u *videoUploads) load(ctx context.Context, hash string) (videoUploadTicket, bool, error) {
	if u.redis != nil {
		raw, err := u.redis.Get(ctx, u.key(hash)).Bytes()
		if errors.Is(err, redis.Nil) {
			return videoUploadTicket{}, false, nil
		}
		if err != nil {
			return videoUploadTicket{}, false, err
		}
		var ticket videoUploadTicket
		if json.Unmarshal(raw, &ticket) != nil {
			return videoUploadTicket{}, false, nil
		}
		return ticket, true, nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	ticket, ok := u.local[hash]
	return ticket, ok, nil
}

// claim reserves a ticket for one PUT: a second upload with the same token
// is refused even while the first is still streaming.
func (u *videoUploads) claim(ctx context.Context, hash string) (bool, error) {
	if u.redis != nil {
		return u.redis.SetNX(ctx, u.key(hash)+":claim", "1", videoUploadTicketTTL).Result()
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.local[hash+":claim"].Received {
		return false, nil
	}
	u.local[hash+":claim"] = videoUploadTicket{Received: true}
	return true, nil
}

func (u *videoUploads) release(ctx context.Context, hash string) {
	if u.redis != nil {
		_ = u.redis.Del(ctx, u.key(hash)+":claim").Err()
		return
	}
	u.mu.Lock()
	delete(u.local, hash+":claim")
	u.mu.Unlock()
}

func hashUploadToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func validUploadToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

func (h *Handler) uploads() *videoUploads {
	h.uploadsOnce.Do(func() {
		h.videoUploads = &videoUploads{}
		if h.lb != nil && h.lb.Store != nil {
			h.videoUploads.redis, h.videoUploads.prefix = h.lb.Store.RedisClient(), h.lb.Store.RedisPrefix()
		}
	})
	return h.videoUploads
}

func uploadDir(cfg *config.Config) string {
	base := filepath.Join("data", "tmp")
	if cfg != nil && strings.TrimSpace(cfg.MediaDir) != "" {
		base = strings.TrimSpace(cfg.MediaDir)
	}
	return filepath.Join(base, "uploads")
}

// fallbackUploadBase returns the configured public origin, or "" when the
// fallback is disabled. Only an https origin is usable: xAI uploads over TLS.
func fallbackUploadBase(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.GrokBuildVideoUploadBaseURL), "/")
	parsed, err := url.Parse(base)
	if base == "" || err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	return base
}

func fallbackAPIBase(cfg *config.Config) string {
	if cfg != nil && strings.TrimSpace(cfg.GrokBuildFallbackBaseURL) != "" {
		return strings.TrimRight(strings.TrimSpace(cfg.GrokBuildFallbackBaseURL), "/")
	}
	return defaultBuildFallbackBaseURL
}

// issueVideoUpload creates a one-time upload URL and returns it with the
// ticket hash the job waits on.
func (h *Handler) issueVideoUpload(ctx context.Context, cfg *config.Config) (uploadURL, hash string, err error) {
	base := fallbackUploadBase(cfg)
	if base == "" {
		return "", "", errors.New("the Build video fallback needs grok_build_video_upload_base_url")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	token := hex.EncodeToString(raw)
	hash = hashUploadToken(token)
	ticket := videoUploadTicket{Path: filepath.Join(uploadDir(cfg), hash+".video"), ExpiresAt: time.Now().Add(videoUploadTicketTTL)}
	if err := h.uploads().save(ctx, hash, ticket); err != nil {
		return "", "", err
	}
	return base + "/grok/v1" + VideoUploadPath + token, hash, nil
}

// waitVideoUpload waits for the ticket's PUT and returns the bytes.
func (h *Handler) waitVideoUpload(ctx context.Context, hash string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, videoUploadWait)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		ticket, ok, err := h.uploads().load(ctx, hash)
		if err != nil {
			return nil, "", err
		}
		if ok && ticket.Received {
			data, err := os.ReadFile(ticket.Path)
			_ = os.Remove(ticket.Path)
			if err != nil {
				return nil, "", fmt.Errorf("read the uploaded video: %w", err)
			}
			return data, ticket.MimeType, nil
		}
		select {
		case <-ctx.Done():
			return nil, "", errVideoUploadNotReceived
		case <-ticker.C:
		}
	}
}

// HandleVideoUpload receives xAI's PUT of a finished fallback video. The
// token in the path is the authorization; no API key is involved.
func (h *Handler) HandleVideoUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	index := strings.LastIndex(r.URL.Path, VideoUploadPath)
	token := ""
	if index >= 0 {
		token = r.URL.Path[index+len(VideoUploadPath):]
	}
	if !validUploadToken(token) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	hash := hashUploadToken(token)
	ctx := r.Context()
	ticket, ok, err := h.uploads().load(ctx, hash)
	switch {
	case err != nil:
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		return
	case time.Now().After(ticket.ExpiresAt):
		w.WriteHeader(http.StatusGone)
		return
	case ticket.Received:
		w.WriteHeader(http.StatusConflict)
		return
	}
	if claimed, err := h.uploads().claim(ctx, hash); err != nil || !claimed {
		w.WriteHeader(http.StatusConflict)
		return
	}
	stored := false
	defer func() {
		if !stored {
			h.uploads().release(context.WithoutCancel(ctx), hash)
		}
	}()
	// A video takes longer than the server's whole-request read timeout.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(videoUploadReadDeadline))
	mimeType, status := receiveVideoFile(r.Body, ticket.Path)
	if status != http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	ticket.Received, ticket.MimeType = true, mimeType
	if err := h.uploads().save(context.WithoutCancel(ctx), hash, ticket); err != nil {
		_ = os.Remove(ticket.Path)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	stored = true
	w.WriteHeader(http.StatusNoContent)
}

// receiveVideoFile streams an upload to path, bounded and sniffed: only a
// video is kept.
func receiveVideoFile(body io.Reader, path string) (string, int) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", http.StatusInternalServerError
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
	if err != nil {
		return "", http.StatusInternalServerError
	}
	defer func() { _ = os.Remove(temp.Name()) }()
	head := make([]byte, 512)
	n, readErr := io.ReadFull(body, head)
	head = head[:n]
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		_ = temp.Close()
		return "", http.StatusBadRequest
	}
	mimeType := http.DetectContentType(head)
	if !strings.HasPrefix(mimeType, "video/") {
		_ = temp.Close()
		return "", http.StatusUnsupportedMediaType
	}
	written, err := io.Copy(temp, io.MultiReader(strings.NewReader(string(head)), io.LimitReader(body, videoUploadMaxBytes-int64(len(head))+1)))
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return "", http.StatusBadRequest
	}
	if written > videoUploadMaxBytes {
		return "", http.StatusRequestEntityTooLarge
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return "", http.StatusInternalServerError
	}
	return mimeType, http.StatusNoContent
}

// fallbackEligible reports whether a refused Build video may retry on the
// xAI API: the fallback is configured, the refusal is a 403, and the account
// is on a paid plan (Free accounts have no xAI video entitlement).
func fallbackEligible(cfg *config.Config, acc *store.Account, err error) bool {
	return fallbackUploadBase(cfg) != "" && upstreamHTTPResponseStatus(err) == http.StatusForbidden && accountpolicy.GrokBuildAccountIsPaid(acc)
}

// runFallbackVideo creates the video on the xAI API and waits for its upload.
func (b buildVideoBackend) runFallbackVideo(ctx context.Context, job web.VideoBackendJob, acc *store.Account) (web.VideoBackendResult, error) {
	h := b.h
	cfg := h.configSnapshot()
	uploadURL, hash, err := h.issueVideoUpload(ctx, cfg)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	payload, err := buildVideoPayload(job)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	payload["model"] = buildFallbackVideoModel
	// The xAI API names a reference image's address "url".
	if references, ok := payload["reference_images"].([]map[string]any); ok {
		for _, reference := range references {
			reference["url"] = reference["image_url"]
			delete(reference, "image_url")
		}
	}
	payload["output"] = map[string]any{"upload_url": uploadURL}
	body, _ := json.Marshal(payload)
	client := h.buildClient()
	base := fallbackAPIBase(cfg)
	headers := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"xai-grok-build/" + client.clientVersion()}}
	response, err := client.request(ctx, acc, http.MethodPost, base+"/videos/generations", body, headers)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	raw, err := grokbase.ReadVideoBody(response)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	requestID, err := grokbase.ParseVideoCreate(raw)
	if err != nil {
		return web.VideoBackendResult{}, err
	}
	slog.Info("Build video created through the xAI API fallback", "account_id", acc.ID, "request_id", requestID)
	job.Bind(0, "", requestID)
	for {
		response, err := client.request(ctx, acc, http.MethodGet, base+"/videos/"+url.PathEscape(requestID), nil, http.Header{"User-Agent": headers["User-Agent"]})
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		raw, err := grokbase.ReadVideoBody(response)
		if err != nil {
			return web.VideoBackendResult{}, err
		}
		// The status may carry no URL: the video is delivered by upload.
		videoURL, done, statusErr := grokbase.ParseVideoStatus(raw, job.Progress)
		if statusErr != nil && !strings.Contains(statusErr.Error(), "without a content URL") {
			return web.VideoBackendResult{}, statusErr
		}
		if done || statusErr != nil {
			data, mimeType, waitErr := h.waitVideoUpload(ctx, hash)
			if waitErr == nil {
				return web.VideoBackendResult{URL: videoURL, Data: data, MimeType: mimeType}, nil
			}
			if videoURL != "" {
				data, mimeType, err := grokbase.DownloadVideo(ctx, client.httpClient, videoURL)
				if err != nil {
					return web.VideoBackendResult{}, err
				}
				return web.VideoBackendResult{URL: videoURL, Data: data, MimeType: mimeType}, nil
			}
			return web.VideoBackendResult{}, waitErr
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
