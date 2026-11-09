package web

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"encoding/json"
	"orchids-api/internal/httpserver"
	"orchids-api/internal/store"
	"orchids-api/internal/util"
)

const defaultAdminMediaPageSize = 24

type adminVideoJob struct {
	ID                string    `json:"id"`
	AccountID         int64     `json:"account_id,omitempty"`
	Provider          string    `json:"provider,omitempty"`
	Model             string    `json:"model"`
	Prompt            string    `json:"prompt,omitempty"`
	Seconds           int       `json:"seconds,omitempty"`
	Size              string    `json:"size,omitempty"`
	Quality           string    `json:"quality,omitempty"`
	Status            string    `json:"status"`
	Progress          int       `json:"progress"`
	VideoURL          string    `json:"video_url,omitempty"`
	ContentURL        string    `json:"content_url,omitempty"`
	DownloadURL       string    `json:"download_url,omitempty"`
	UpstreamRequestID string    `json:"upstream_request_id,omitempty"`
	RemixedFromID     string    `json:"remixed_from_id,omitempty"`
	Operation         string    `json:"operation,omitempty"`
	StandardAPI       bool      `json:"standard_api,omitempty"`
	ErrorCode         string    `json:"error_code,omitempty"`
	ErrorMessage      string    `json:"error_message,omitempty"`
	CreatedAt         int64     `json:"created_at"`
	CompletedAt       int64     `json:"completed_at,omitempty"`
	ExpiresAt         time.Time `json:"expires_at,omitempty"`
	UpdatedAt         time.Time `json:"updated_at,omitempty"`
}

type adminImageDeleteRequest struct {
	Name  string   `json:"name"`
	Names []string `json:"names"`
}

type adminVideoDeleteRequest struct {
	ID string `json:"id"`
}

func parseAdminMediaPagination(r *http.Request) (int, int) {
	page := parsePositiveInt(r.URL.Query().Get("page"), 1)
	pageSize := min(parsePositiveInt(r.URL.Query().Get("page_size"), defaultAdminMediaPageSize), maxCachePageSize)
	return page, pageSize
}

func containsFold(value, query string) bool {
	return query == "" || strings.Contains(strings.ToLower(value), query)
}
func paginateAdminImages(entries []cacheEntry, page, pageSize int) ([]cacheEntry, int) {
	return paginateCacheEntries(entries, page, pageSize)
}
func paginateAdminVideos(jobs []adminVideoJob, page, pageSize int) ([]adminVideoJob, int) {
	total := len(jobs)
	if total == 0 || page < 1 || pageSize < 1 || page > (total-1)/pageSize+1 {
		return []adminVideoJob{}, total
	}
	start := (page - 1) * pageSize
	return jobs[start:min(total, start+pageSize)], total
}

func adminMediaDescending(r *http.Request) bool {
	return strings.ToLower(strings.TrimSpace(r.URL.Query().Get("order"))) != "asc"
}

func sortAdminImages(entries []cacheEntry, key string, desc bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	sort.SliceStable(entries, func(i, j int) bool {
		cmp := 0
		switch key {
		case "name":
			cmp = strings.Compare(strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name))
		case "size":
			if entries[i].SizeBytes < entries[j].SizeBytes {
				cmp = -1
			} else if entries[i].SizeBytes > entries[j].SizeBytes {
				cmp = 1
			}
		default:
			if entries[i].UpdatedAt < entries[j].UpdatedAt {
				cmp = -1
			} else if entries[i].UpdatedAt > entries[j].UpdatedAt {
				cmp = 1
			}
		}
		if desc {
			return cmp > 0
		}
		return cmp < 0
	})
}

func (h *Handler) HandleAdminMediaImages(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	entries, _, err := listCachedEntries("image")
	if err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to list images")
		return
	}
	query := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("search"), r.URL.Query().Get("q"))))
	filtered := make([]cacheEntry, 0, len(entries))
	for _, entry := range entries {
		if containsFold(entry.Name, query) {
			entry.ViewURL = "/api/grok/admin/media/images/content/" + entry.Name
			entry.PreviewURL = entry.ViewURL
			entry.URL = entry.ViewURL
			filtered = append(filtered, entry)
		}
	}
	entries = filtered
	sortAdminImages(entries, r.URL.Query().Get("sort"), adminMediaDescending(r))
	page, pageSize := parseAdminMediaPagination(r)
	items, total := paginateAdminImages(entries, page, pageSize)
	util.WriteJSON(w, map[string]any{"status": "success", "items": items, "total": total, "page": page, "page_size": pageSize})
}

func adminImageNameFromPath(path string) string {
	marker := "/media/images/content/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return ""
	}
	raw := strings.Trim(strings.TrimPrefix(path[idx:], marker), "/")
	name := sanitizeCachedFilename(raw)
	if name != raw || strings.HasPrefix(name, mediaInputFilePrefix) {
		return ""
	}
	return name
}

func (h *Handler) HandleAdminMediaImageContent(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	name := adminImageNameFromPath(r.URL.Path)
	if name == "" {
		httpserver.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	path := filepath.Join(cacheBaseDir, "image", name)
	file, err := os.Open(path)
	if err != nil {
		httpserver.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		httpserver.WriteError(w, http.StatusNotFound, "image not found")
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (h *Handler) HandleAdminMediaImagesDelete(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	var req adminImageDeleteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		httpserver.WriteError(w, http.StatusBadRequest, "invalid delete request")
		return
	}
	names := append([]string(nil), req.Names...)
	if strings.TrimSpace(req.Name) != "" {
		names = append(names, req.Name)
	}
	seen := map[string]bool{}
	removed := 0
	for _, raw := range names {
		name := sanitizeCachedFilename(strings.TrimSpace(raw))
		if name == "" || name != strings.TrimSpace(raw) || strings.HasPrefix(name, mediaInputFilePrefix) || seen[name] {
			continue
		}
		seen[name] = true
		if err := os.Remove(filepath.Join(cacheBaseDir, "image", name)); err == nil {
			removed++
		} else if !os.IsNotExist(err) {
			httpserver.WriteError(w, http.StatusInternalServerError, "failed to delete image")
			return
		}
	}
	if len(seen) == 0 {
		httpserver.WriteError(w, http.StatusBadRequest, "no valid image names")
		return
	}
	util.WriteJSON(w, map[string]any{"status": "success", "removed_count": removed})
}

func (h *Handler) HandleAdminMediaImageStats(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	entries, size, err := listCachedEntries("image")
	if err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to read image stats")
		return
	}
	util.WriteJSON(w, map[string]any{"status": "success", "count": len(entries), "bytes": size, "size_bytes": size, "size_mb": bytesToMB(size)})
}

func storedJobForAdmin(job *store.StoredVideoJob) adminVideoJob {
	if job == nil {
		return adminVideoJob{}
	}
	out := adminVideoJob{ID: job.ID, AccountID: job.AccountID, Provider: job.Provider, Model: job.Model, Prompt: job.Prompt, Seconds: job.Seconds, Size: job.Size, Quality: job.Quality, Status: job.Status, Progress: job.Progress, VideoURL: job.VideoURL, UpstreamRequestID: job.UpstreamRequestID, RemixedFromID: job.RemixedFromID, Operation: job.Operation, StandardAPI: job.StandardAPI, ErrorCode: job.ErrorCode, ErrorMessage: job.ErrorMessage, CreatedAt: job.CreatedAt, CompletedAt: job.CompletedAt, ExpiresAt: job.ExpiresAt, UpdatedAt: job.UpdatedAt}
	if job.Status == "completed" && validPersistedVideoContentPath(job.ContentPath) && strings.TrimSpace(job.ContentPath) != "" {
		out.ContentURL = "/api/grok/admin/media/videos/content/" + job.ID
		out.DownloadURL = out.ContentURL + "?download=1"
	}
	return out
}

func (h *Handler) listAdminVideoJobs(r *http.Request) ([]adminVideoJob, error) {
	jobs, err := h.lb.Store.ListStoredVideoJobs(r.Context())
	if err != nil {
		return nil, err
	}
	query := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("search"), r.URL.Query().Get("q"))))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	out := make([]adminVideoJob, 0, len(jobs))
	for _, job := range jobs {
		if job == nil || (status != "" && strings.ToLower(strings.TrimSpace(job.Status)) != status) {
			continue
		}
		if query != "" && !containsFold(job.ID, query) && !containsFold(job.Prompt, query) && !containsFold(job.Model, query) && !containsFold(job.Provider, query) {
			continue
		}
		out = append(out, storedJobForAdmin(job))
	}
	key, desc := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort"))), adminMediaDescending(r)
	sort.SliceStable(out, func(i, j int) bool {
		cmp := 0
		switch key {
		case "status":
			cmp = strings.Compare(out[i].Status, out[j].Status)
		case "model":
			cmp = strings.Compare(out[i].Model, out[j].Model)
		case "created":
			if out[i].CreatedAt < out[j].CreatedAt {
				cmp = -1
			} else if out[i].CreatedAt > out[j].CreatedAt {
				cmp = 1
			}
		default:
			if out[i].UpdatedAt.Before(out[j].UpdatedAt) {
				cmp = -1
			} else if out[i].UpdatedAt.After(out[j].UpdatedAt) {
				cmp = 1
			}
		}
		if desc {
			return cmp > 0
		}
		return cmp < 0
	})
	return out, nil
}

func (h *Handler) findAdminVideoJob(r *http.Request, id string) (*store.StoredVideoJob, error) {
	jobs, err := h.lb.Store.ListStoredVideoJobs(r.Context())
	if err != nil {
		return nil, err
	}
	for _, job := range jobs {
		if job != nil && job.ID == id {
			return job, nil
		}
	}
	return nil, store.ErrNoRows
}

func adminVideoIDFromPath(path string) string {
	marker := "/media/videos/content/"
	idx := strings.Index(path, marker)
	if idx < 0 {
		return ""
	}
	id := strings.Trim(strings.TrimPrefix(path[idx:], marker), "/")
	if id == "" || strings.Contains(id, "/") {
		return ""
	}
	return id
}
func (h *Handler) HandleAdminMediaVideoContent(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	id := adminVideoIDFromPath(r.URL.Path)
	job, err := h.findAdminVideoJob(r, id)
	if err != nil || job.Status != "completed" || !validPersistedVideoContentPath(job.ContentPath) || strings.TrimSpace(job.ContentPath) == "" {
		httpserver.WriteError(w, http.StatusNotFound, "video content not found")
		return
	}
	file, err := os.Open(job.ContentPath)
	if err != nil {
		httpserver.WriteError(w, http.StatusNotFound, "video content not found")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		httpserver.WriteError(w, http.StatusNotFound, "video content not found")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	disposition := "inline"
	if r.URL.Query().Get("download") == "1" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Disposition", disposition+`; filename="`+sanitizeCachedFilename(id)+`.mp4"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, id+".mp4", info.ModTime(), file)
}

func terminalVideoStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "cancelled", "canceled":
		return true
	}
	return false
}
func (h *Handler) HandleAdminMediaVideoDelete(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodPost) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	var req adminVideoDeleteRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req) != nil || strings.TrimSpace(req.ID) == "" {
		httpserver.WriteError(w, http.StatusBadRequest, "video id is required")
		return
	}
	job, err := h.findAdminVideoJob(r, strings.TrimSpace(req.ID))
	if errors.Is(err, store.ErrNoRows) {
		httpserver.WriteError(w, http.StatusNotFound, "video job not found")
		return
	}
	if err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to read video job")
		return
	}
	if !terminalVideoStatus(job.Status) {
		httpserver.WriteError(w, http.StatusConflict, "only terminal video jobs can be deleted")
		return
	}
	if err = h.lb.Store.DeleteStoredVideoJob(r.Context(), job.ID, job.OwnerHash); err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to delete video job")
		return
	}
	if strings.TrimSpace(job.ContentPath) != "" && validPersistedVideoContentPath(job.ContentPath) {
		_ = os.Remove(job.ContentPath)
	}
	util.WriteJSON(w, map[string]any{"status": "success", "deleted": job.ID})
}

func (h *Handler) HandleAdminMediaVideos(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	jobs, err := h.listAdminVideoJobs(r)
	if err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to list video jobs")
		return
	}
	page, pageSize := parseAdminMediaPagination(r)
	items, total := paginateAdminVideos(jobs, page, pageSize)
	util.WriteJSON(w, map[string]any{"status": "success", "items": items, "total": total, "page": page, "page_size": pageSize})
}
func (h *Handler) HandleAdminMediaVideoStats(w http.ResponseWriter, r *http.Request) {
	if !httpserver.RequireMethod(w, r, http.MethodGet) {
		return
	}
	if h == nil || h.lb == nil || h.lb.Store == nil {
		httpserver.WriteError(w, http.StatusServiceUnavailable, "store not configured")
		return
	}
	jobs, err := h.lb.Store.ListStoredVideoJobs(r.Context())
	if err != nil {
		httpserver.WriteError(w, http.StatusInternalServerError, "failed to read video stats")
		return
	}
	statuses := map[string]int{}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		status := strings.ToLower(strings.TrimSpace(job.Status))
		if status == "" {
			status = "unknown"
		}
		statuses[status]++
	}
	util.WriteJSON(w, map[string]any{"status": "success", "total": len(jobs), "statuses": statuses})
}
